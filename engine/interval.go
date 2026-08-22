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
	"go.uber.org/zap"
)

const setIntervalFunctionName = "n6kd_set_interval"

// intervalTask owns the connection its body runs on, from the inline run at
// registration through every tick. DuckDB scopes SET VARIABLE to a connection,
// so this is what lets a body set a variable and read it back with getvariable
// in a later statement of the same body, and on the next tick. Secrets are
// instance-wide, so a CREATE OR REPLACE SECRET here reaches everything.
type intervalTask struct {
	name  string
	every time.Duration
	body  string
	conn  *sql.Conn
	// Serializes ticks against each other and against the inline run, for a body
	// that takes longer than its interval.
	mu sync.Mutex
	// Non-nil when the source is not client-reachable, which is what makes
	// getenv safe to expose to the body.
	env *getenvUDF
}

func (t *intervalTask) run(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.env != nil {
		t.env.enterBody()
		defer t.env.exitBody()
	}
	_, err := t.conn.ExecContext(ctx, t.body)
	return err
}

// setIntervalUDF is n6kd_set_interval(name, INTERVAL, sql) -> VARCHAR. Armed
// only while the boot SQL runs: the source stays registered afterwards (and in
// direct mode is reachable from client SQL), so post-boot calls are refused
// rather than letting a client schedule SQL at the source's trust level.
//
// The body runs inline, before the call returns, so the statements after it in
// the boot SQL see its effect. It runs on the task's own connection rather than
// the pooled one the calling statement holds: a capped pool would otherwise
// deadlock here with no connection left to take. The calling statement's own
// snapshot predates the body, so only the statements after it observe the rows.
type setIntervalUDF struct {
	mu    sync.Mutex
	armed bool
	// Non-zero while a body is executing, which is what makes a body's own call
	// to n6kd_set_interval an error instead of a deadlock behind its task mutex.
	running int
	tasks   []*intervalTask
	// Connections of tasks whose body failed. Closing one inside the UDF
	// callback would disconnect from a DuckDB worker thread while the calling
	// statement is still running on the same instance, so disarm does it once
	// boot is off the stack.
	dead []*intervalTask
	// A body's own failure, surfaced by takeBootErr once the statement that
	// called it has finished. Returning it through the UDF would propagate an
	// inner query's error out of a DuckDB worker thread while the outer
	// statement is still running on the same instance.
	bootErr error
	source  *sql.DB
	//nolint:containedctx // the boot context, for connections opened from a UDF callback.
	ctx context.Context
	env *getenvUDF
	log *zap.SugaredLogger
}

// arm opens registration. env is nil in direct mode, where a body must not be
// able to read the process environment.
func (u *setIntervalUDF) arm(ctx context.Context, db *sql.DB, env *getenvUDF) error {
	// Each task holds a connection for the life of the engine, and the boot
	// statement that registers it holds one of its own.
	if limit := db.Stats().MaxOpenConnections; limit > 0 && limit < 2 {
		return fmt.Errorf("%s needs spare connections: the source pool allows %d",
			setIntervalFunctionName, limit)
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.armed, u.source, u.ctx, u.env = true, db, ctx, env
	return nil
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

			task, err := newIntervalTask(name, every, body)
			if err != nil {
				return nil, err
			}
			ctx, err := u.begin(task)
			if err != nil {
				return nil, err
			}
			if failed := u.finish(task, task.run(ctx)); failed {
				return fmt.Sprintf("%s failed", task.name), nil
			}
			u.log.Infof("interval %q: ran at boot, repeats every %v", task.name, task.every)
			return fmt.Sprintf("%s every %v", task.name, task.every), nil
		},
	}
}

func newIntervalTask(name string, every duckdb.Interval, body string) (*intervalTask, error) {
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
	return &intervalTask{name: name, every: duration, body: body}, nil
}

// begin admits one registration and opens the connection its body will keep.
// The task is not recorded until finish sees the body succeed.
func (u *setIntervalUDF) begin(task *intervalTask) (context.Context, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !u.armed {
		return nil, errors.New("n6kd_set_interval is only available in boot SQL (-f/-c)")
	}
	if u.running > 0 {
		return nil, fmt.Errorf("n6kd_set_interval(%q): cannot be called while another body runs", task.name)
	}
	for _, existing := range u.tasks {
		if existing.name == task.name {
			return nil, fmt.Errorf("n6kd_set_interval(%q): already registered", task.name)
		}
	}
	// Not u.ctx: the connection outlives the boot, and a cancelled context would
	// have database/sql close it underneath the ticker. The body itself still
	// runs under u.ctx, so a cancelled boot aborts it.
	conn, err := u.source.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	task.conn, task.env = conn, u.env
	u.running++
	return u.ctx, nil
}

// finish reports whether the body failed. The error is held for takeBootErr
// rather than returned into DuckDB.
func (u *setIntervalUDF) finish(task *intervalTask, bodyErr error) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.running--
	if bodyErr != nil {
		u.dead = append(u.dead, task)
		if u.bootErr == nil {
			u.bootErr = fmt.Errorf("n6kd_set_interval(%q): %w", task.name, bodyErr)
		}
		return true
	}
	u.tasks = append(u.tasks, task)
	return false
}

// takeBootErr returns the first body failure since the last call, for the
// caller to fail the boot with once its statement has returned.
func (u *setIntervalUDF) takeBootErr() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	err := u.bootErr
	u.bootErr = nil
	return err
}

// disarm closes registration and releases the connections of tasks that failed.
// The tasks it returns keep theirs until the engine closes them.
func (u *setIntervalUDF) disarm() ([]*intervalTask, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.armed, u.source, u.ctx = false, nil, nil
	dead := u.dead
	u.dead = nil
	return u.tasks, closeTasks(dead)
}

func (e *Engine) startIntervalTasks(tasks []*intervalTask) {
	e.tasks = tasks
	if len(tasks) == 0 {
		return
	}
	e.done = make(chan struct{})
	for _, task := range tasks {
		e.wg.Add(1)
		go func(task *intervalTask) {
			defer e.wg.Done()
			ticker := time.NewTicker(task.every)
			defer ticker.Stop()
			for {
				select {
				case <-e.done:
					return
				case <-ticker.C:
					if err := task.run(context.Background()); err != nil {
						e.log.Errorf("interval %q: %v", task.name, err)
					} else {
						e.log.Debugf("interval %q: ran", task.name)
					}
				}
			}
		}(task)
	}
}

// closeIntervalTasks releases the per-task connections. Call after the tickers
// have stopped.
func (e *Engine) closeIntervalTasks() error {
	err := closeTasks(e.tasks)
	e.tasks = nil
	return err
}

// closeTasks releases connections on a boot that will not produce an engine.
func closeTasks(tasks []*intervalTask) error {
	var errs []error
	for _, task := range tasks {
		errs = append(errs, task.conn.Close())
	}
	return errors.Join(errs...)
}

// closeAfterFailedBoot releases every connection the registrar opened, for a
// boot that will not produce an engine.
func closeAfterFailedBoot(u *setIntervalUDF) error {
	tasks, err := u.disarm()
	return errors.Join(err, closeTasks(tasks))
}
