package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"

	"github.com/n6k-io/n6kd/internal/streamframe"
	"github.com/n6k-io/n6kd/internal/wstext"
)

type wsLink struct {
	ws    *websocket.Conn
	text  *wstext.Conn
	host  *os.File
	extFD int

	done     chan struct{}
	doneOnce sync.Once
	err      error
}

func dialLink(ctx context.Context, url string) (*wsLink, error) {
	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	ws.SetReadLimit(streamframe.MaxBytes)

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("socketpair: %w", err), ws.Close())
	}
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		return nil, errors.Join(fmt.Errorf("set the host socket non-blocking: %w", err),
			syscall.Close(fds[0]), syscall.Close(fds[1]), ws.Close())
	}

	l := &wsLink{
		ws:    ws,
		text:  wstext.New(ws),
		host:  os.NewFile(uintptr(fds[0]), "n6k-host"),
		extFD: fds[1],
		done:  make(chan struct{}),
	}
	go l.finishWith(l.wsToSock)
	go l.finishWith(l.sockToWS)
	return l, nil
}

func (l *wsLink) finishWith(pump func() error) {
	err := pump()
	l.doneOnce.Do(func() {
		l.err = err
		close(l.done)
	})
}

func (l *wsLink) alive() bool {
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

func (l *wsLink) close(releaseExtFD bool) error {
	_ = l.text.Close()
	wsErr := l.ws.Close()
	hostErr := l.host.Close()
	<-l.done
	if !releaseExtFD {
		return errors.Join(wsErr, hostErr)
	}
	return errors.Join(wsErr, hostErr, syscall.Close(l.extFD))
}

func (l *wsLink) wsToSock() error {
	for {
		messageType, payload, err := l.ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
				errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		switch messageType {
		case websocket.BinaryMessage:
		case websocket.TextMessage:
			l.text.Deliver(payload)
			continue
		default:
			continue
		}
		if err := streamframe.Write(l.host, messageType, payload); err != nil {
			return err
		}
	}
}

func (l *wsLink) sockToWS() error {
	reader := bufio.NewReader(l.host)
	for {
		messageType, payload, err := streamframe.Read(reader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
		if err := l.text.WriteMessage(messageType, payload); err != nil {
			return err
		}
	}
}
