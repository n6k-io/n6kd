package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/server"
)

// socketMode is bound before the boot so a bad path fails fast while the
// parent's dials queue. A nil serve means it is off.
type socketMode struct {
	serve func(ctx context.Context, eng *engine.Engine, log *zap.SugaredLogger) error
	close func() error
}

// openSocketMode binds --listen / N6K_LISTEN. The parent on the other end owns
// auth, so any auth flag is a contradiction.
func openSocketMode(authn authFlags, listenPath string) (socketMode, error) {
	none := socketMode{close: func() error { return nil }}
	if listenPath == "" {
		return none, nil
	}
	if authn.apiKey != "" || authn.issuerURL != "" || authn.audience != "" {
		return none, fmt.Errorf("--api-key, --oidc-issuer-url and --oidc-audience cannot be used with --listen (%s): the parent dialing in authenticates",
			server.ListenEnv)
	}
	ln, err := server.ListenSocket(listenPath)
	if err != nil {
		return none, err
	}
	return socketMode{
		serve: func(ctx context.Context, eng *engine.Engine, log *zap.SugaredLogger) error {
			log.Infof("serving %s on %s %s", strings.Join(eng.Catalogs(), ", "), server.ListenEnv, listenPath)
			return server.ServeListen(ctx, ln, eng, log)
		},
		close: func() error {
			if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
				return err
			}
			return nil
		},
	}, nil
}
