package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

const setIntervalFunctionName = "n6kd_set_interval"

type intervalTask struct {
	name  string
	every time.Duration
	body  string
}

// setIntervalUDF is n6kd_set_interval(name, INTERVAL, sql) -> VARCHAR. Armed
// only while the boot SQL runs: the source stays registered afterwards (and in
// direct mode is reachable from client SQL), so post-boot calls are refused
// rather than letting a client schedule SQL at the source's trust level.
type setIntervalUDF struct {
	mu    sync.Mutex
	armed bool
	tasks []intervalTask
}

func (u *setIntervalUDF) Config() duckdb.ScalarFuncConfig {
	varchar, err := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	if err != nil {
		panic(err)
	}
	interval, err := duckdb.NewTypeInfo(duckdb.TYPE_INTERVAL)
	if err != nil {
		panic(err)
	}
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{varchar, interval, varchar},
		ResultTypeInfo: varchar,
	}
}

func (u *setIntervalUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			name, _ := values[0].(string)
			every, _ := values[1].(duckdb.Interval)
			body, _ := values[2].(string)

			if name == "" {
				return nil, errors.New("n6kd_set_interval: name is empty")
			}
			if body == "" {
				return nil, fmt.Errorf("n6kd_set_interval(%q): sql is empty", name)
			}
			// A month has no fixed length, so it makes no duration to tick at.
			if every.Months != 0 {
				return nil, fmt.Errorf("n6kd_set_interval(%q): interval must not use months or years", name)
			}
			duration := time.Duration(every.Days)*24*time.Hour + time.Duration(every.Micros)*time.Microsecond
			if duration <= 0 {
				return nil, fmt.Errorf("n6kd_set_interval(%q): interval must be positive", name)
			}

			u.mu.Lock()
			defer u.mu.Unlock()
			if !u.armed {
				return nil, errors.New("n6kd_set_interval is only available in boot SQL (-f/-c)")
			}
			for _, task := range u.tasks {
				if task.name == name {
					return nil, fmt.Errorf("n6kd_set_interval(%q): already registered", name)
				}
			}
			u.tasks = append(u.tasks, intervalTask{name: name, every: duration, body: body})
			return fmt.Sprintf("%s every %v", name, duration), nil
		},
	}
}

func (u *setIntervalUDF) disarm() []intervalTask {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.armed = false
	return u.tasks
}

func registerSetIntervalUDF(ctx context.Context, db *sql.DB, udf *setIntervalUDF) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if err := duckdb.RegisterScalarUDF(conn, setIntervalFunctionName, udf); err != nil {
		return fmt.Errorf("register %s: %w", setIntervalFunctionName, err)
	}
	return nil
}

// runIntervalTasksOnce executes each body against the source, so a broken body
// fails the boot rather than the first tick.
func (e *Engine) runIntervalTasksOnce(ctx context.Context, tasks []intervalTask) error {
	for _, task := range tasks {
		if _, err := e.source.ExecContext(ctx, task.body); err != nil {
			return fmt.Errorf("n6kd_set_interval(%q): %w", task.name, err)
		}
		e.log.Infof("interval %q: ran at boot, repeats every %v", task.name, task.every)
	}
	return nil
}

func (e *Engine) startIntervalTasks(tasks []intervalTask) {
	if len(tasks) == 0 {
		return
	}
	e.done = make(chan struct{})
	for _, task := range tasks {
		e.wg.Add(1)
		go func(task intervalTask) {
			defer e.wg.Done()
			ticker := time.NewTicker(task.every)
			defer ticker.Stop()
			for {
				select {
				case <-e.done:
					return
				case <-ticker.C:
					if _, err := e.source.ExecContext(context.Background(), task.body); err != nil {
						e.log.Errorf("interval %q: %v", task.name, err)
					} else {
						e.log.Debugf("interval %q: ran", task.name)
					}
				}
			}
		}(task)
	}
}
