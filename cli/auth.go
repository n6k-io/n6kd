package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/auth"
	"github.com/n6k-io/n6kd/engine"
)

type authFlags struct {
	apiKey, issuerURL, audience string
}

func (f authFlags) verifier(ctx context.Context, log *zap.SugaredLogger) (engine.TokenVerifier, error) {
	if f.issuerURL == "" && f.audience == "" {
		if f.apiKey == "" {
			log.Warnf("no --api-key or --oidc-issuer-url given: any client that can reach the port is served")
		} else {
			log.Infof("clients must present the --api-key token")
		}
		return nil, nil
	}
	if f.apiKey != "" {
		return nil, fmt.Errorf("--api-key and --oidc-issuer-url cannot be used together")
	}
	if f.issuerURL == "" || f.audience == "" {
		return nil, fmt.Errorf("--oidc-issuer-url and --oidc-audience must be given together")
	}
	verifier, err := auth.NewOIDC(ctx, f.issuerURL, f.audience)
	if err != nil {
		return nil, err
	}
	log.Infof("clients must present a JWT from %s for audience %q", f.issuerURL, f.audience)
	return verifier, nil
}

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage the credentials n6kd mcp connects with",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newLoginCmd())
	return cmd
}

func newLoginCmd() *cobra.Command {
	var issuer, clientID, resource string
	var debugLogs, jsonLogs bool

	cmd := &cobra.Command{
		Use:   "login --issuer URL [--client-id ID] [--resource AUD]",
		Short: "Log in with n6k_login and store the persistent n6k_refresh secret",
		Long: "Runs n6k_login, the n6k_client device login, which stores a persistent\n" +
			"n6k_refresh secret. n6kd mcp mints its connect token from that secret.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if issuer == "" {
				return errors.New("--issuer is required")
			}

			log, err := installLogger(debugLogs, jsonLogs)
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			db, err := engine.OpenClient(cmd.Context(), log)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			query := "SELECT status, detail FROM n6k_login(?"
			args := []any{issuer}
			if clientID != "" {
				query += ", client_id => ?"
				args = append(args, clientID)
			}
			if resource != "" {
				query += ", resource => ?"
				args = append(args, resource)
			}
			var status, detail string
			if err := db.QueryRowContext(cmd.Context(), query+")", args...).Scan(&status, &detail); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", status, detail)
			return err
		},
	}

	cmd.Flags().StringVar(&issuer, "issuer", os.Getenv("N6KD_AUTH_ISSUER"), "authorization server base URL")
	cmd.Flags().StringVar(&clientID, "client-id", os.Getenv("N6KD_AUTH_CLIENT_ID"), "client ID (n6k_login default when unset)")
	cmd.Flags().StringVar(&resource, "resource", os.Getenv("N6KD_AUTH_RESOURCE"),
		"audience of the minted token; the server's --oidc-audience")
	cmd.Flags().BoolVar(&debugLogs, "debug", os.Getenv("N6KD_DEBUG") != "", "log extension detail")
	cmd.Flags().BoolVar(&jsonLogs, "json-logging", os.Getenv("N6KD_JSON_LOGGING") != "",
		"log JSON lines with Cloud Logging severity fields, for log collectors")
	return cmd
}
