package auth_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/n6k-io/n6kd/auth"
	"github.com/n6k-io/n6kd/auth/authtest"
)

func TestOIDCReturnsScalarClaims(t *testing.T) {
	issuer := authtest.NewIssuer(t)
	verifier, err := auth.NewOIDC(t.Context(), issuer.URL, "n6kd")
	if err != nil {
		t.Fatal(err)
	}

	claims, err := verifier.Verify(t.Context(), issuer.Token(t, "n6kd", map[string]any{
		"sub":    "alice",
		"admin":  true,
		"level":  3,
		"weight": 1.5,
		"roles":  []string{"a", "b"},
		"meta":   map[string]any{"k": "v"},
	}))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for name, want := range map[string]any{"sub": "alice", "admin": true, "level": int64(3), "weight": 1.5} {
		if got := claims[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", name, got, want)
		}
	}
	for _, name := range []string{"roles", "meta"} {
		if _, ok := claims[name]; ok {
			t.Errorf("%s should be dropped, got %#v", name, claims[name])
		}
	}
}

func TestOIDCRejectsBadTokens(t *testing.T) {
	issuer := authtest.NewIssuer(t)
	other := authtest.NewIssuer(t)
	verifier, err := auth.NewOIDC(t.Context(), issuer.URL, "n6kd")
	if err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{
		"wrong audience": issuer.Token(t, "someone-else", map[string]any{"sub": "alice"}),
		"expired": issuer.Token(t, "n6kd", map[string]any{
			"sub": "alice",
			"exp": time.Now().Add(-time.Hour).Unix(),
		}),
		"other issuer":  other.Token(t, "n6kd", map[string]any{"sub": "alice"}),
		"forged issuer": other.Token(t, "n6kd", map[string]any{"sub": "alice", "iss": issuer.URL}),
		"not a jwt":     "s3cret",
		"empty":         "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(t.Context(), token); err == nil {
				t.Error("verified, want an error")
			}
		})
	}
}

func TestNewOIDCNeedsIssuerAndAudience(t *testing.T) {
	issuer := authtest.NewIssuer(t)
	if _, err := auth.NewOIDC(t.Context(), issuer.URL, ""); err == nil {
		t.Error("no audience accepted")
	}
	if _, err := auth.NewOIDC(t.Context(), "", "n6kd"); err == nil {
		t.Error("no issuer accepted")
	}
}
