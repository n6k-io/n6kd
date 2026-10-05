package cli

import (
	"context"
	"fmt"

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
