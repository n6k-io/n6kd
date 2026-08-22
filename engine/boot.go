package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"go.uber.org/zap"
)

// runBootSQL runs the -f files and -c statements in the order given. getenv is
// armed for a file and not for a statement, which has the shell's own
// substitution. A failure here aborts the boot, so the caller closes the source.
func runBootSQL(ctx context.Context, source *sql.DB, boot []Step,
	env *getenvUDF, intervals *setIntervalUDF, log *zap.SugaredLogger,
) error {
	for i, step := range boot {
		text, label := step.SQL, fmt.Sprintf("-c #%d", i+1)
		if step.Path != "" {
			// #nosec G304 -- the operator names this file on the command line.
			raw, err := os.ReadFile(step.Path)
			if err != nil {
				return err
			}
			text, label = string(raw), step.Path
		}
		env.setArmed(step.Path != "")
		if _, err := source.ExecContext(ctx, text); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		// A body that failed left its error here rather than returning it into
		// the statement that called it.
		if err := intervals.takeBootErr(); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		log.Infof("ran %s (%d bytes)", label, len(text))
	}
	return nil
}
