package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
)

type OIDC struct {
	verifier *oidc.IDTokenVerifier
}

func NewOIDC(ctx context.Context, issuerURL, audience string) (*OIDC, error) {
	if issuerURL == "" || audience == "" {
		return nil, fmt.Errorf("oidc needs both an issuer URL and an audience")
	}
	provider, err := oidc.NewProvider(context.WithoutCancel(ctx), issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", issuerURL, err)
	}
	return &OIDC{verifier: provider.Verifier(&oidc.Config{ClientID: audience})}, nil
}

func (o *OIDC) Verify(ctx context.Context, token string) (map[string]any, error) {
	verified, err := o.verifier.Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := verified.Claims(&raw); err != nil {
		return nil, err
	}
	return scalarClaims(raw)
}

func scalarClaims(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var all map[string]any
	if err := dec.Decode(&all); err != nil {
		return nil, err
	}
	claims := map[string]any{}
	for name, value := range all {
		switch v := value.(type) {
		case string, bool:
			claims[name] = v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				claims[name] = n
			} else if f, err := v.Float64(); err == nil {
				claims[name] = f
			}
		}
	}
	return claims, nil
}
