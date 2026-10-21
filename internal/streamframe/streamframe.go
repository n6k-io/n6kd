// Package streamframe is the n6k stream framing on the extension's fd:
// [4-byte BE payload length][1-byte type][payload]. Type values are the
// websocket text/binary opcodes, so a websocket message type is a frame type.
package streamframe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/gorilla/websocket"
)

const (
	JSONRPC = websocket.TextMessage
	N6K     = websocket.BinaryMessage
)

// Bounds a corrupt length prefix; far above any real frame.
const MaxBytes = 1 << 30

func Write(w io.Writer, typ int, payload []byte) error {
	if typ != JSONRPC && typ != N6K {
		return fmt.Errorf("unknown stream frame type %d", typ)
	}
	if len(payload) > MaxBytes {
		return fmt.Errorf("frame of %d bytes exceeds the %d byte limit", len(payload), MaxBytes)
	}
	var header [5]byte
	// #nosec G115 -- the check above bounds this by MaxBytes.
	binary.BigEndian.PutUint32(header[:4], uint32(len(payload)))
	header[4] = byte(typ) // #nosec G115 -- typ is JSONRPC or N6K, checked above.
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// Read returns io.EOF when r ends anywhere inside a frame's header.
func Read(r io.Reader) (typ int, payload []byte, err error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, io.EOF
		}
		return 0, nil, err
	}
	typ = int(header[4])
	if typ != JSONRPC && typ != N6K {
		return 0, nil, fmt.Errorf("unknown stream frame type 0x%02x from the extension", header[4])
	}
	size := binary.BigEndian.Uint32(header[:4])
	if size > MaxBytes {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds the %d byte limit", size, MaxBytes)
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return typ, payload, nil
}
