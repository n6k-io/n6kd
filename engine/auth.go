package engine

import (
	"context"
	"crypto/subtle"
	"errors"
)

var ErrUnauthorized = errors.New("unauthorized")

type TokenVerifier interface {
	Verify(ctx context.Context, token string) (map[string]any, error)
}

func (e *Engine) Authenticate(ctx context.Context, token string) (map[string]any, error) {
	if e.opts.Verifier != nil {
		if token == "" {
			return nil, ErrUnauthorized
		}
		claims, err := e.opts.Verifier.Verify(ctx, token)
		if err != nil {
			return nil, errors.Join(ErrUnauthorized, err)
		}
		return claims, nil
	}
	if e.opts.APIKey == "" {
		return nil, nil
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(e.opts.APIKey)) != 1 {
		return nil, ErrUnauthorized
	}
	return nil, nil
}
