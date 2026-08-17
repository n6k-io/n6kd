package server

import (
	"net"
	"testing"
)

// The ready line is meant to be pasted into a client, so an address that only
// makes sense to the listener has to be rewritten before it is printed.
func TestDialableAuthority(t *testing.T) {
	tests := []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{Port: 8080}, "localhost:8080"},
		{&net.TCPAddr{IP: net.IPv6zero, Port: 8080}, "localhost:8080"},
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9000}, "127.0.0.1:9000"},
		{&net.TCPAddr{IP: net.IPv4(10, 0, 0, 4), Port: 80}, "10.0.0.4:80"},
	}
	for _, test := range tests {
		if got := dialableAuthority(test.addr); got != test.want {
			t.Errorf("dialableAuthority(%s) = %q, want %q", test.addr, got, test.want)
		}
	}
}
