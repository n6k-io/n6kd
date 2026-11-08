package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/n6k-io/n6kd/internal/sqlquote"
	"github.com/n6k-io/n6kd/server"
)

const metadataTimeout = 10 * time.Second

func loginToServer(cmd *cobra.Command, db *sql.DB, target string) error {
	ctx := cmd.Context()
	scope, metadataURL, err := protectedResourceURL(target)
	if err != nil {
		return err
	}
	resource, err := fetchProtectedResource(ctx, metadataURL)
	if err != nil {
		return err
	}
	refreshToken, tokenEndpoint, err := deviceLogin(ctx, resource, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	name := "n6kd_login_" + unsafeSecretChars.ReplaceAllString(scope, "_")
	stmt := fmt.Sprintf("CREATE OR REPLACE PERSISTENT SECRET %s (TYPE n6k_refresh, REFRESH_TOKEN %s, TOKEN_ENDPOINT %s, CLIENT_ID %s, SCOPE %s)",
		sqlquote.Ident(name), sqlquote.String(refreshToken), sqlquote.String(tokenEndpoint),
		sqlquote.String(resource.ClientID), sqlquote.String(scope))
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("store the login for %s: %w", scope, err)
	}
	_, err = fmt.Fprintf(cmd.ErrOrStderr(), "signed in: stored persistent n6k_refresh secret %s for %s\n", name, scope)
	return err
}

// protectedResourceURL returns the secret scope and metadata URL for a server
// named as host[:port] or any of its URLs. A bare host is https unless loopback.
func protectedResourceURL(target string) (scope, metadataURL string, err error) {
	raw := target
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("server %q: %w", target, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("server %q has no host", target)
	}
	var scheme string
	switch u.Scheme {
	case "http", "ws", "n6k":
		scheme = "http"
	case "https", "wss", "n6ks":
		scheme = "https"
	case "":
		scheme = "https"
		if isLoopback(u.Hostname()) {
			scheme = "http"
		}
	default:
		return "", "", fmt.Errorf("server %q has unsupported scheme %q", target, u.Scheme)
	}
	return u.Host, scheme + "://" + u.Host + server.ProtectedResourcePath, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func fetchProtectedResource(ctx context.Context, metadataURL string) (server.ProtectedResource, error) {
	var resource server.ProtectedResource
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, http.NoBody)
	if err != nil {
		return resource, err
	}
	// #nosec G704 -- the operator names this server on the command line.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return resource, fmt.Errorf("reach the server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return resource, fmt.Errorf("%s not found: the server must run with --oidc-client-id to offer sign-in", metadataURL)
	}
	if resp.StatusCode != http.StatusOK {
		return resource, fmt.Errorf("%s: HTTP %d", metadataURL, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&resource); err != nil {
		return resource, fmt.Errorf("%s: %w", metadataURL, err)
	}
	if len(resource.AuthorizationServers) == 0 || resource.ClientID == "" {
		return resource, fmt.Errorf("%s: authorization_servers and client_id are required", metadataURL)
	}
	return resource, nil
}

func deviceLogin(ctx context.Context, resource server.ProtectedResource, out io.Writer) (refreshToken, tokenEndpoint string, err error) {
	issuer := resource.AuthorizationServers[0]
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return "", "", fmt.Errorf("oidc discovery for %s: %w", issuer, err)
	}
	endpoint := provider.Endpoint()
	if endpoint.DeviceAuthURL == "" {
		return "", "", fmt.Errorf("%s does not advertise a device_authorization_endpoint", issuer)
	}
	endpoint.AuthStyle = oauth2.AuthStyleInParams

	var discovered struct {
		ScopesSupported []string `json:"scopes_supported"`
	}
	if err := provider.Claims(&discovered); err != nil {
		return "", "", fmt.Errorf("oidc discovery for %s: %w", issuer, err)
	}
	conf := oauth2.Config{ClientID: resource.ClientID, Endpoint: endpoint, Scopes: loginScopes(discovered.ScopesSupported)}

	auth, err := conf.DeviceAuth(ctx)
	if err != nil {
		return "", "", fmt.Errorf("start device login at %s: %w", endpoint.DeviceAuthURL, err)
	}
	if _, err := fmt.Fprintf(out, "Sign in at %s (code: %s)\n", auth.VerificationURI, auth.UserCode); err != nil {
		return "", "", err
	}
	token, err := conf.DeviceAccessToken(ctx, auth)
	if err != nil {
		return "", "", fmt.Errorf("device login: %w", err)
	}
	if token.RefreshToken == "" {
		return "", "", errors.New("the issuer returned no refresh token, so the login cannot be kept")
	}
	return token.RefreshToken, endpoint.TokenURL, nil
}

// loginScopes drops what an issuer's published list excludes: Google, for one,
// rejects offline_access. An issuer with no list gets them all.
func loginScopes(supported []string) []string {
	scopes := []string{oidc.ScopeOpenID, "email", oidc.ScopeOfflineAccess}
	if len(supported) == 0 {
		return scopes
	}
	return slices.DeleteFunc(scopes, func(scope string) bool { return !slices.Contains(supported, scope) })
}
