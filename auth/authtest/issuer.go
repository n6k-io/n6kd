package authtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type Issuer struct {
	URL    string
	signer jose.Signer
	// LoginAudience and LoginClaims are what tokens from the device and refresh grants carry.
	LoginAudience string
	LoginClaims   map[string]any
}

func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "test-key"
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}

	issuer := &Issuer{signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"issuer":                                issuer.URL,
			"jwks_uri":                              issuer.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"EdDSA"},
			"device_authorization_endpoint":         issuer.URL + "/device",
			"token_endpoint":                        issuer.URL + "/token",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"device_code":      "device-code",
			"user_code":        "TEST-CODE",
			"verification_uri": issuer.URL + "/activate",
			"expires_in":       60,
			"interval":         1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		switch r.PostForm.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code", "refresh_token":
			writeJSON(t, w, map[string]any{
				"access_token":  issuer.Token(t, issuer.LoginAudience, issuer.LoginClaims),
				"token_type":    "Bearer",
				"refresh_token": "refresh-token",
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(t, w, map[string]any{"error": "unsupported_grant_type"})
		}
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: pub, KeyID: kid, Algorithm: string(jose.EdDSA), Use: "sig"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer.URL = srv.URL
	return issuer
}

func (i *Issuer) Token(t *testing.T, audience string, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	all := map[string]any{
		"iss": i.URL,
		"aud": audience,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	maps.Copy(all, claims)
	token, err := jwt.Signed(i.signer).Claims(all).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}
