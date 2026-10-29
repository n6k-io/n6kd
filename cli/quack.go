package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/n6k-io/n6kd/engine"
)

func newQuackCmd() *cobra.Command {
	var f bootFlags
	var addr, token string

	cmd := &cobra.Command{
		Use:   "quack -f schema.sql [-f file.sql | -c SQL]... [--perms perms.yml]",
		Short: "Serve the loaded database over DuckDB's quack protocol",
		Long: "Runs the -f files and -c statements, in the order given, into a DuckDB\n" +
			"database and serves it to DuckDB clients over quack (ATTACH 'quack:host:port').\n\n" +
			"With --perms, serves the one catalog perms.yml names, bridged. Without, serves\n" +
			"the loaded database directly, read and write.\n\n" +
			"Every client presents the same --token; there are no users. Quack speaks plain\n" +
			"HTTP: put TLS in a proxy in front of it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			cmd.SilenceUsage = true

			if err := f.resolve(cmd); err != nil {
				return err
			}

			log, err := installLogger(f.debug, f.jsonLogs)
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			if err := f.requireFiles(); err != nil {
				return err
			}
			uri, allowOtherHost, err := quackURI(addr)
			if err != nil {
				return err
			}

			started := time.Now()
			opts, err := f.options(log)
			if err != nil {
				return err
			}
			opts.Quack = true

			eng, err := engine.Open(cmd.Context(), opts, log)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, eng.Close()) }()

			if f.bootOnly {
				log.Infof("--boot-only: boot SQL ran, not serving")
				return nil
			}

			url, authToken, err := eng.ServeQuack(cmd.Context(), uri, token, allowOtherHost)
			if err != nil {
				return err
			}
			log.Infof("startup finished in %v", time.Since(started).Round(time.Millisecond))
			if token == "" {
				log.Warnf("no --token given: clients must present the generated token %s", authToken)
			}
			log.Infof("serving %v over quack at %s", eng.Catalogs(), url)

			<-cmd.Context().Done()
			log.Infof("shutting down")
			return nil
		},
	}

	f.register(cmd.Flags(), "for the one served database")
	cmd.Flags().StringVar(&addr, "addr", defaultAddr(), "address to listen on; a host other than localhost listens beyond this machine")
	cmd.Flags().StringVar(&token, "token", os.Getenv("N6KD_QUACK_TOKEN"),
		"token every client must present, at least 4 characters; generated and logged when empty")
	return cmd
}

// quackURI turns host:port into quack's URI. Quack refuses any host but
// localhost unless told otherwise; an empty host listens on every interface.
func quackURI(addr string) (uri string, allowOtherHost bool, err error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false, fmt.Errorf("--addr: %w", err)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return "quack:" + net.JoinHostPort(host, port), host != "localhost", nil
}
