package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
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
	return newRootCmd().ExecuteContext(ctx)
}

func newRootCmd() *cobra.Command {
	var cfg server.Config
	var apiKey string
	var lockdown, noLockdown bool
	var debug bool

	cmd := &cobra.Command{
		Use:   "n6kd <schema.sql> [perms.yml]",
		Short: "Serve DuckDB catalogs over the n6k protocol",
		Long: "Loads <schema.sql> into a DuckDB database and serves the catalogs named in\n" +
			"<perms.yml> to WebSocket clients.\n\n" +
			"Either path may be omitted when N6K_SCHEMA / N6K_PERMS name it instead,\n" +
			"which is how the container image passes what a child image COPYs in.\n\n" +
			"With no perms.yml at all, clients are served from the loaded database\n" +
			"directly: every catalog it holds, read and write, unbridged.",
		Args: cobra.MaximumNArgs(2),
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

			log, err := installLogger(debug)
			if err != nil {
				return err
			}
			// Nothing is buffered on stderr, so a failure to sync is not worth
			// failing the run over.
			defer func() { _ = log.Sync() }()

			cfg.SchemaPath, err = requirePathArg(args, 0, "N6K_SCHEMA", "schema.sql")
			if err != nil {
				return err
			}
			cfg.PermsPath = optionalPathArg(args, 1, "N6K_PERMS")

			paths := []string{cfg.SchemaPath}
			if cfg.PermsPath != "" {
				paths = append(paths, cfg.PermsPath)
			}
			if err := requireExistingFiles(paths...); err != nil {
				return err
			}

			// All of this runs before server.Run binds the listener, so a bad schema
			// or a bad perms.yml fails the boot instead of the first request.
			started := time.Now()

			var rules *perms.File
			if cfg.PermsPath == "" {
				log.Warnf("no perms.yml given: serving the loaded database directly, with no permission layer")
			} else {
				rules, err = perms.Load(cfg.PermsPath)
				if err != nil {
					return err
				}
				log.Infof("loaded perms %s (source %s, %s)",
					cfg.PermsPath,
					rules.Source.Catalog+"."+rules.Source.Schema,
					plural(len(rules.Catalogs), "catalog"))
			}

			if lockdown {
				log.Infof("lockdown on: served connections cannot reach the filesystem")
			} else {
				log.Warnf("lockdown off: a client's SQL can read and write this host's files")
			}

			if apiKey == "" {
				log.Warnf("no --api-key given: any client that can reach the port is served")
			} else {
				log.Infof("clients must present the --api-key token")
			}

			eng, err := engine.Open(cmd.Context(), engine.Options{
				SchemaPath: cfg.SchemaPath,
				Perms:      rules,
				APIKey:     apiKey,
				Lockdown:   lockdown,
			}, log)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, eng.Close()) }()
			log.Infof("startup finished in %v", time.Since(started).Round(time.Millisecond))

			return server.Run(cmd.Context(), cfg, eng, log)
		},
	}

	cmd.AddCommand(newInstallCmd(), newSelfTestCmd())

	cmd.Flags().StringVar(&cfg.Addr, "addr", defaultAddr(), "address to listen on")
	cmd.Flags().StringVar(&apiKey, "api-key", os.Getenv("N6K_API_KEY"),
		"token clients must present to connect; without one, anyone who reaches the port is served")
	cmd.Flags().BoolVar(&lockdown, "lockdown", os.Getenv("N6K_NO_LOCKDOWN") == "",
		"take the local filesystem and extension installation away from served connections")
	cmd.Flags().BoolVar(&noLockdown, "no-lockdown", false, "the inverse of --lockdown")
	cmd.Flags().BoolVar(&debug, "debug", os.Getenv("N6K_DEBUG") != "", "log per-connection and per-catalog detail")
	cmd.SetErrPrefix("n6kd:")
	return cmd
}

// newInstallCmd downloads the extensions ahead of any serving. The Dockerfile
// runs it at build time so the image ships with them already on disk.
func newInstallCmd() *cobra.Command {
	var debug bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Download the n6k DuckDB extensions into DuckDB's extension directory",
		Long: "Installs the extensions this server loads (virtual_catalog, n6k_client, n6k_server) from\n" +
			"the release bucket. Set " + engine.ExtensionHomeEnv + " to choose where they land and\n" +
			engine.ExtensionRepoEnv + " to install from somewhere other than the default bucket.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			log, err := installLogger(debug)
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			return engine.InstallExtensions(cmd.Context(), log)
		},
	}

	cmd.Flags().BoolVar(&debug, "debug", os.Getenv("N6K_DEBUG") != "", "log per-extension detail")
	return cmd
}

// newSelfTestCmd exercises the serving path against built-in fixtures. The
// Dockerfile runs it as the final build step.
func newSelfTestCmd() *cobra.Command {
	var debug bool

	cmd := &cobra.Command{
		Use:   "selftest",
		Short: "Load the extensions and bridge a throwaway catalog end to end",
		Long: "Builds a small source database from built-in fixtures, bridges a session,\n" +
			"and checks that a bridged read returns rows and a write to a read-only table\n" +
			"is refused. Uses no schema.sql or perms.yml of your own.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			log, err := installLogger(debug)
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

	cmd.Flags().BoolVar(&debug, "debug", os.Getenv("N6K_DEBUG") != "", "log per-extension and per-catalog detail")
	return cmd
}

// requirePathArg resolves a positional path, falling back to the environment so
// the container image can supply it without a wrapper script.
func requirePathArg(args []string, i int, env, name string) (string, error) {
	if i < len(args) {
		return args[i], nil
	}
	if path := os.Getenv(env); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("no %s given: pass it as argument %d or set %s", name, i+1, env)
}

func optionalPathArg(args []string, i int, env string) string {
	if i < len(args) {
		return args[i]
	}
	return os.Getenv(env)
}

// defaultAddr honours PORT, which hosts like Cloud Run set to choose the port.
// --addr still wins, and takes a host as well.
func defaultAddr() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func installLogger(debug bool) (*zap.SugaredLogger, error) {
	cfg := zap.NewDevelopmentConfig()
	if debug {
		cfg.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	} else {
		cfg.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	}
	// The file:line of the log statement is noise for an operator; the message
	// says where it came from. A stack trace on an error line is the same.
	cfg.DisableCaller = true
	cfg.DisableStacktrace = true
	cfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
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
