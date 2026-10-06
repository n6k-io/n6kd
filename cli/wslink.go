package cli

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
)

const maxLinkFrameBytes = 1 << 30

type wsLink struct {
	ws    *websocket.Conn
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
	ws.SetReadLimit(maxLinkFrameBytes)

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
	wsErr := l.ws.Close()
	hostErr := l.host.Close()
	<-l.done
	if !releaseExtFD {
		return errors.Join(wsErr, hostErr)
	}
	return errors.Join(wsErr, hostErr, syscall.Close(l.extFD))
}

func (l *wsLink) wsToSock() error {
	var header [4]byte
	for {
		messageType, payload, err := l.ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
				errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		size := len(payload)
		if size > math.MaxUint32 {
			return fmt.Errorf("server frame of %d bytes is too large", size)
		}
		binary.BigEndian.PutUint32(header[:], uint32(size))
		if _, err := l.host.Write(header[:]); err != nil {
			return err
		}
		if _, err := l.host.Write(payload); err != nil {
			return err
		}
	}
}

func (l *wsLink) sockToWS() error {
	reader := bufio.NewReader(l.host)
	var header [4]byte
	for {
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(header[:])
		if size > maxLinkFrameBytes {
			return fmt.Errorf("frame of %d bytes exceeds the %d byte limit", size, maxLinkFrameBytes)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return err
		}
		if err := l.ws.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			return err
		}
	}
}
