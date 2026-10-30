package server

import (
	"crypto/rand"
	"testing"
)

func TestLookupHostRejectsAnUnknownSession(t *testing.T) {
	unknown := make([]byte, 16)
	if _, err := rand.Read(unknown); err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{unknown, int64(3), nil} {
		if h, err := lookupHost(v); err == nil {
			t.Errorf("lookupHost(%v) found %v, want an error", v, h)
		}
	}
}
