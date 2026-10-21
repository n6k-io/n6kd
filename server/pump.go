package server

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"

	"github.com/gorilla/websocket"

	"github.com/n6k-io/n6kd/internal/streamframe"
	"github.com/n6k-io/n6kd/internal/wstext"
)

func wsToSock(ws *websocket.Conn, text *wstext.Conn, mcpGranted *atomic.Bool, host *os.File, counts *counters) error {
	for {
		messageType, payload, err := ws.ReadMessage()
		if err != nil {
			// A client going away — cleanly, or by dropping the connection — is an
			// ordinary end of stream, and so is our own teardown closing this socket
			// out from under the read. Anything else is a real failure to report.
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
				websocket.CloseAbnormalClosure,
			) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		switch messageType {
		case websocket.BinaryMessage:
		case websocket.TextMessage:
			if mcpGranted != nil {
				if mcpGranted.Load() {
					text.Deliver(payload)
				}
				continue
			}
		default:
			continue
		}
		if err := streamframe.Write(host, messageType, payload); err != nil {
			return err
		}
		counts.framesIn.Add(1)
		counts.bytesIn.Add(int64(len(payload)))
	}
}

func sockToWS(text *wstext.Conn, host *os.File, counts *counters) error {
	reader := bufio.NewReader(host)
	for {
		messageType, payload, err := streamframe.Read(reader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // the extension closed
			}
			return err
		}
		if err := text.WriteMessage(messageType, payload); err != nil {
			return err
		}
		counts.framesOut.Add(1)
		counts.bytesOut.Add(int64(len(payload)))
	}
}
