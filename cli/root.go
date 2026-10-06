package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/perms"
	"github.com/n6k-io/n6kd/server"
)

func Execute() error {
	// Ctrl-C reaches Run through the command context, which is what drives the
	// graceful shutdown rather than killing in-flight requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := loadDotenv(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "n6kd:", err)
		return err
	}
	return newRootCmd().ExecuteContext(ctx)
}

func newRootCmd() *cobra.Command {
	var cfg server.Config
	var permsPath string
	var authn authFlags
	var lockdown, noLockdown bool
	var debug, jsonLogs bool
	var bootOnly bool

	var boot []engine.Step

	cmd := &cobra.Command{
		Use:   "n6kd -f schema.sql [-f file.sql | -c SQL]... [--perms perms.yml]",
		Short: "Serve DuckDB catalogs over the n6k protocol",
		Long: "Runs the -f files and -c statements, in the order given, into a DuckDB\n" +
			"database and serves the catalogs named in --perms to WebSocket clients.\n\n" +
			"With no perms.yml at all, clients are served from the loaded database\n" +
			"directly: every catalog it holds, read and write, unbridged.",
		Args: cobra.NoArgs,
		// Named return so the deferred close can report a failure to flush or
		// release the database rather than drop it.
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			// Set here, not on the command: the arguments already parsed, so anything
			// that fails past this point is a runtime error and usage would be noise.
			cmd.SilenceUsage = true

			if noLockdown {
				if cmd.Flags().Changed("lockdown") && lockdown {
					return fmt.Errorf("--lockdown and --no-lockdown contradict each other")
				}
				lockdown = false
			}

			log, err := installLogger(debug, jsonLogs)
			if err != nil {
				return err
			}
			// Nothing is buffered on stderr, so a failure to sync is not worth
			// failing the run over.
			defer func() { _ = log.Sync() }()

			if len(boot) == 0 {
				return fmt.Errorf("no boot SQL given: pass -f or -c")
			}

			var paths []string
			for _, step := range boot {
				if step.Path != "" {
					paths = append(paths, step.Path)
				}
			}
			if permsPath != "" {
				paths = append(paths, permsPath)
			}
			if err := requireExistingFiles(paths...); err != nil {
				return err
			}

			// All of this runs before server.Run binds the listener, so a bad schema
			// or a bad perms.yml fails the boot instead of the first request.
			started := time.Now()

			var rules *perms.File
			if permsPath == "" {
				log.Warnf("no perms.yml given: serving the loaded database directly, with no permission layer")
			} else {
				rules, err = perms.Load(permsPath)
				if err != nil {
					return err
				}
				log.Infof("loaded perms %s (%s)", permsPath, plural(len(rules.Catalogs), "catalog"))
			}

			if lockdown {
				log.Infof("lockdown on: served connections cannot reach the filesystem")
			} else {
				log.Warnf("lockdown off: a client's SQL can read and write this host's files")
			}

			verifier, err := authn.verifier(cmd.Context(), log)
			if err != nil {
				return err
			}

			eng, err := engine.Open(cmd.Context(), engine.Options{
				Boot:     boot,
				Perms:    rules,
				APIKey:   authn.apiKey,
				Verifier: verifier,
				Lockdown: lockdown,
			}, log)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, eng.Close()) }()
			log.Infof("startup finished in %v", time.Since(started).Round(time.Millisecond))

			if bootOnly {
				log.Infof("--boot-only: boot SQL ran, not serving")
				return nil
			}

			return server.Run(cmd.Context(), cfg, eng, log)
		},
	}

	cmd.AddCommand(newInstallCmd(), newSelfTestCmd(), newMCPCmd(), newAuthCmd())
	addDotenvFlag(cmd.PersistentFlags())

	cmd.Flags().VarP(&stepFlag{steps: &boot, file: true}, "file", "f",
		"SQL file run into the source database at boot; repeatable, in order with -c")
	cmd.Flags().VarP(&stepFlag{steps: &boot}, "command", "c",
		"SQL run into the source database at boot; repeatable, in order with -f")
	cmd.Flags().StringVar(&permsPath, "perms", "", "perms.yml naming the catalogs to serve bridged")
	cmd.Flags().StringVar(&cfg.Addr, "addr", defaultAddr(), "address to listen on")
	cmd.Flags().StringSliceVar(&cfg.AllowedOrigins, "allow-origin", defaultAllowedOrigins(),
		"browser origins allowed to connect (WebSocket handshake and CORS); repeatable, default any")
	cmd.Flags().StringVar(&authn.apiKey, "api-key", os.Getenv("N6KD_API_KEY"),
		"token clients must present to connect; without one, anyone who reaches the port is served")
	cmd.Flags().StringVar(&authn.issuerURL, "oidc-issuer-url", os.Getenv("N6KD_OIDC_ISSUER_URL"),
		"OIDC issuer whose JWTs clients must present; their claims become $name context in perms.yml")
	cmd.Flags().StringVar(&authn.audience, "oidc-audience", os.Getenv("N6KD_OIDC_AUDIENCE"),
		"aud claim a client's JWT must carry; required with --oidc-issuer-url")
	cmd.Flags().BoolVar(&lockdown, "lockdown", os.Getenv("N6KD_NO_LOCKDOWN") == "",
		"take the local filesystem and extension installation away from served connections")
	cmd.Flags().BoolVar(&noLockdown, "no-lockdown", false, "the inverse of --lockdown")
	cmd.Flags().BoolVar(&bootOnly, "boot-only", false,
		"run the -f files and -c statements, then exit without binding the listener")
	cmd.Flags().BoolVar(&debug, "debug", os.Getenv("N6KD_DEBUG") != "", "log per-connection and per-catalog detail")
	cmd.Flags().BoolVar(&jsonLogs, "json-logging", os.Getenv("N6KD_JSON_LOGGING") != "",
		"log JSON lines with Cloud Logging severity fields, for log collectors")
	cmd.SetErrPrefix("n6kd:")
	return cmd
}

// newInstallCmd downloads the extensions ahead of any serving. The Dockerfile
// runs it at build time so the image ships with them already on disk.
func newInstallCmd() *cobra.Command {
	var debug, jsonLogs bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Download the n6k DuckDB extensions into DuckDB's extension directory",
		Long: "Installs the extensions this server loads (virtual_catalog_bridge, n6k_client, n6k_server) from\n" +
			"the release bucket. Set " + engine.ExtensionHomeEnv + " to choose where they land and\n" +
			engine.ExtensionRepoEnv + " to install from somewhere other than the default bucket.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			log, err := installLogger(debug, jsonLogs)
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			return engine.InstallExtensions(cmd.Context(), log)
		},
	}

	cmd.Flags().BoolVar(&debug, "debug", os.Getenv("N6KD_DEBUG") != "", "log per-extension detail")
	cmd.Flags().BoolVar(&jsonLogs, "json-logging", os.Getenv("N6KD_JSON_LOGGING") != "",
		"log JSON lines with Cloud Logging severity fields, for log collectors")
	return cmd
}

// newSelfTestCmd exercises the serving path against built-in fixtures. The
// Dockerfile runs it as the final build step.
func newSelfTestCmd() *cobra.Command {
	var debug, jsonLogs bool

	cmd := &cobra.Command{
		Use:   "selftest",
		Short: "Load the extensions and bridge a throwaway catalog end to end",
		Long: "Builds a small source database from built-in fixtures, bridges a session,\n" +
			"and checks that a bridged read returns rows and a write to a read-only table\n" +
			"is refused. Uses no schema.sql or perms.yml of your own.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			log, err := installLogger(debug, jsonLogs)
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			if err := engine.SelfTest(cmd.Context(), log); err != nil {
				return err
			}
			log.Infof("selftest passed")
			return nil
		},
	}

	cmd.Flags().BoolVar(&debug, "debug", os.Getenv("N6KD_DEBUG") != "", "log per-extension and per-catalog detail")
	cmd.Flags().BoolVar(&jsonLogs, "json-logging", os.Getenv("N6KD_JSON_LOGGING") != "",
		"log JSON lines with Cloud Logging severity fields, for log collectors")
	return cmd
}

// stepFlag appends to a slice shared between -f and -c, so the boot steps keep
// the order the flags were given in rather than grouping by flag.
type stepFlag struct {
	steps *[]engine.Step
	file  bool
}

func (s *stepFlag) Set(v string) error {
	if s.file {
		*s.steps = append(*s.steps, engine.Step{Path: v})
	} else {
		*s.steps = append(*s.steps, engine.Step{SQL: v})
	}
	return nil
}

func (s *stepFlag) String() string { return "" }

func (s *stepFlag) Type() string {
	if s.file {
		return "file"
	}
	return "sql"
}

// defaultAddr honours PORT, which hosts like Cloud Run set to choose the port.
// --addr still wins, and takes a host as well.
func defaultAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

func defaultAllowedOrigins() []string {
	if v := os.Getenv("N6KD_ALLOW_ORIGIN"); v != "" {
		origins := strings.Split(v, ",")
		for i, o := range origins {
			origins[i] = strings.TrimSpace(o)
		}
		return origins
	}
	return nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// gcpSeverity maps zap levels onto Cloud Logging's severity values, which is
// what makes a JSON line render at the right level in the GCP console.
func gcpSeverity(level zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	switch level {
	case zapcore.DebugLevel:
		enc.AppendString("DEBUG")
	case zapcore.InfoLevel:
		enc.AppendString("INFO")
	case zapcore.WarnLevel:
		enc.AppendString("WARNING")
	case zapcore.ErrorLevel:
		enc.AppendString("ERROR")
	case zapcore.DPanicLevel:
		enc.AppendString("CRITICAL")
	case zapcore.PanicLevel:
		enc.AppendString("ALERT")
	default:
		enc.AppendString("EMERGENCY")
	}
}

func installLogger(debug, jsonLogs bool) (*zap.SugaredLogger, error) {
	cfg := zap.NewDevelopmentConfig()
	if jsonLogs {
		cfg = zap.NewProductionConfig()
		// Sampling drops repeats, which is wrong for an operator's log of record.
		cfg.Sampling = nil
		cfg.EncoderConfig.TimeKey = "time"
		cfg.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder
		cfg.EncoderConfig.LevelKey = "severity"
		cfg.EncoderConfig.EncodeLevel = gcpSeverity
		cfg.EncoderConfig.MessageKey = "message"
	}
	if debug {
		cfg.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	} else {
		cfg.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	}
	// The file:line of the log statement is noise for an operator; the message
	// says where it came from. A stack trace on an error line is the same.
	cfg.DisableCaller = true
	cfg.DisableStacktrace = true
	if !jsonLogs {
		cfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
	}
	// stderr for both, so stdout stays free for anything a later subcommand
	// wants to emit as data.
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}

	log, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	// Installed globally too, so a package reaching for zap.L() rather than
	// taking a logger still writes in the same format.
	zap.ReplaceGlobals(log)
	return log.Sugar(), nil
}

func requireExistingFiles(paths ...string) error {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("%s is a directory, want a file", path)
		}
	}
	return nil
}
