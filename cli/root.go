package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/internal/textfmt"
	"github.com/n6k-io/n6kd/perms"
	"github.com/n6k-io/n6kd/server"
)

func Execute() error {
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
	var f bootFlags
	var authn authFlags
	var listenPath string
	var pprofAddr string

	cmd := &cobra.Command{
		Use:   "n6kd -f schema.sql [-f file.sql | -c SQL]... [--perms perms.yml]",
		Short: "Serve DuckDB catalogs over the n6k protocol",
		Long: "Runs the -f files and -c statements, in the order given, into a DuckDB\n" +
			"database and serves the catalogs named in --perms to WebSocket clients.\n\n" +
			"With no perms.yml at all, clients are served from the loaded database\n" +
			"directly: every catalog it holds, read and write, unbridged.\n\n" +
			"With --listen (N6K_LISTEN), listens on that Unix socket instead and serves\n" +
			"every client that connects, trusting the parent that dials in.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			// Set here, not on the command: the arguments already parsed, so anything
			// that fails past this point is a runtime error and usage would be noise.
			cmd.SilenceUsage = true

			if err := f.resolve(cmd); err != nil {
				return err
			}
			if err := cfg.Serve.Validate(); err != nil {
				return err
			}

			log, err := installLogger(f.debug, f.jsonLogs)
			if err != nil {
				return err
			}
			// Nothing is buffered on stderr, so a failure to sync is not worth
			// failing the run over.
			defer func() { _ = log.Sync() }()

			if err := f.requireFiles(); err != nil {
				return err
			}

			// All of this runs before any client is served, so a bad schema or a bad
			// perms.yml fails the boot instead of the first request.
			started := time.Now()

			socket, err := openSocketMode(authn, listenPath)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, socket.close()) }()

			opts, err := f.options(log)
			if err != nil {
				return err
			}

			verifier, err := authn.verifier(cmd.Context(), log)
			if err != nil {
				return err
			}
			opts.APIKey = authn.apiKey
			opts.Verifier = verifier

			eng, err := engine.Open(cmd.Context(), opts, log)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, eng.Close()) }()
			log.Infof("startup finished in %v", time.Since(started).Round(time.Millisecond))

			if f.bootOnly {
				log.Infof("--boot-only: boot SQL ran, not serving")
				return nil
			}

			if pprofAddr != "" {
				if err := startPprof(cmd.Context(), pprofAddr, log); err != nil {
					return err
				}
			}
			if socket.serve != nil {
				return socket.serve(cmd.Context(), eng, cfg.Serve, log)
			}
			return server.Run(cmd.Context(), cfg, eng, log)
		},
	}

	cmd.AddCommand(newInstallCmd(), newSelfTestCmd(), newMCPCmd(), newAuthCmd(), newQuackCmd())
	addDotenvFlag(cmd.PersistentFlags())

	f.register(cmd.Flags(), "per session database")
	cmd.Flags().StringVar(&cfg.Addr, "addr", defaultAddr(), "address to listen on")
	cmd.Flags().StringVar(&pprofAddr, "pprof", os.Getenv("N6KD_PPROF"),
		"address to serve Go's net/http/pprof on; off when empty (N6KD_PPROF)")
	cmd.Flags().StringVar(&listenPath, "listen", os.Getenv(server.ListenEnv),
		"Unix socket to listen on instead of --addr, serving every client that connects; the parent dialing in authenticates")
	cmd.Flags().StringSliceVar(&cfg.AllowedOrigins, "allow-origin", defaultAllowedOrigins(),
		"browser origins allowed to connect (WebSocket handshake and CORS); repeatable, default any")
	registerServeFlags(cmd.Flags(), &cfg.Serve)
	cmd.Flags().StringVar(&authn.apiKey, "api-key", os.Getenv("N6KD_API_KEY"),
		"token clients must present to connect; without one, anyone who reaches the port is served")
	cmd.Flags().StringVar(&authn.issuerURL, "oidc-issuer-url", os.Getenv("N6KD_OIDC_ISSUER_URL"),
		"OIDC issuer whose JWTs clients must present; their claims become $name context in perms.yml")
	cmd.Flags().StringVar(&authn.audience, "oidc-audience", os.Getenv("N6KD_OIDC_AUDIENCE"),
		"aud claim a client's JWT must carry; required with --oidc-issuer-url")
	cmd.SetErrPrefix("n6kd:")
	return cmd
}

// bootFlags are what the root command and quack share: the boot SQL, perms.yml
// and the served database's lockdown.
type bootFlags struct {
	boot                 []engine.Step
	permsPath            string
	lockdown, noLockdown bool
	memoryLimit          string
	threads              int
	debug, jsonLogs      bool
	bootOnly             bool
}

// limitScope says what --memory-limit and --threads apply to.
func (f *bootFlags) register(flags *pflag.FlagSet, limitScope string) {
	flags.VarP(&stepFlag{steps: &f.boot, file: true}, "file", "f",
		"SQL file run into the source database at boot; repeatable, in order with -c")
	flags.VarP(&stepFlag{steps: &f.boot}, "command", "c",
		"SQL run into the source database at boot; repeatable, in order with -f")
	flags.StringVar(&f.permsPath, "perms", "", "perms.yml naming the catalogs to serve bridged")
	flags.BoolVar(&f.lockdown, "lockdown", os.Getenv("N6KD_NO_LOCKDOWN") == "",
		"take the local filesystem and extension installation away from served connections")
	flags.BoolVar(&f.noLockdown, "no-lockdown", false, "the inverse of --lockdown")
	flags.StringVar(&f.memoryLimit, "memory-limit", os.Getenv("N6KD_MEMORY_LIMIT"),
		"memory_limit locked in under --lockdown, "+limitScope+"; default DuckDB's")
	flags.IntVar(&f.threads, "threads", 0,
		"threads locked in under --lockdown, "+limitScope+" (N6KD_THREADS); default DuckDB's")
	flags.BoolVar(&f.bootOnly, "boot-only", false,
		"run the -f files and -c statements, then exit without binding the listener")
	flags.BoolVar(&f.debug, "debug", os.Getenv("N6KD_DEBUG") != "", "log per-connection and per-catalog detail")
	flags.BoolVar(&f.jsonLogs, "json-logging", os.Getenv("N6KD_JSON_LOGGING") != "",
		"log JSON lines with Cloud Logging severity fields, for log collectors")
}

func (f *bootFlags) resolve(cmd *cobra.Command) error {
	if f.noLockdown {
		if cmd.Flags().Changed("lockdown") && f.lockdown {
			return fmt.Errorf("--lockdown and --no-lockdown contradict each other")
		}
		f.lockdown = false
	}
	if env := os.Getenv("N6KD_THREADS"); env != "" && !cmd.Flags().Changed("threads") {
		threads, err := strconv.Atoi(env)
		if err != nil {
			return fmt.Errorf("N6KD_THREADS: %w", err)
		}
		f.threads = threads
	}
	return nil
}

func (f *bootFlags) requireFiles() error {
	if len(f.boot) == 0 {
		return fmt.Errorf("no boot SQL given: pass -f or -c")
	}
	var paths []string
	for _, step := range f.boot {
		if step.Path != "" {
			paths = append(paths, step.Path)
		}
	}
	if f.permsPath != "" {
		paths = append(paths, f.permsPath)
	}
	return requireExistingFiles(paths...)
}

func (f *bootFlags) options(log *zap.SugaredLogger) (engine.Options, error) {
	var rules *perms.File
	if f.permsPath == "" {
		log.Warnf("no perms.yml given: serving the loaded database directly, with no permission layer")
	} else {
		var err error
		rules, err = perms.Load(f.permsPath)
		if err != nil {
			return engine.Options{}, err
		}
		log.Infof("loaded perms %s (%s)", f.permsPath, textfmt.Plural(len(rules.Catalogs), "catalog"))
	}

	if f.lockdown {
		log.Infof("lockdown on: served connections cannot reach the filesystem")
	} else {
		log.Warnf("lockdown off: a client's SQL can read and write this host's files")
	}

	return engine.Options{
		Boot:        f.boot,
		Perms:       rules,
		Lockdown:    f.lockdown,
		MemoryLimit: f.memoryLimit,
		Threads:     f.threads,
	}, nil
}

func newInstallCmd() *cobra.Command {
	var debug, jsonLogs bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Download the n6k DuckDB extensions into DuckDB's extension directory",
		Long: "Installs the extensions this server loads (vcatbridge, n6k_client, n6k_server) from\n" +
			"the release bucket, and quack and httpfs from DuckDB's. Set " + engine.ExtensionHomeEnv + " to choose where they land (default: an extensions directory beside the binary if one exists, else DuckDB's own)\nand " + engine.ExtensionRepoEnv + " to install from somewhere other than the default bucket.",
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
