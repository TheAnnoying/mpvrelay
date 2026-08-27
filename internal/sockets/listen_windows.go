//go:build windows

package sockets

import (
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
)

// Listen creates the listener Seanime will dial as the mpv IPC socket.
// This build is only exercised when developing/testing cmd/mpv on
// Windows directly; the real deployment target for the stub is Linux
// (see listen_unix.go).
func Listen(path string) (net.Listener, error) {
	l, err := winio.ListenPipe(path, nil)
	if err != nil {
		return nil, fmt.Errorf("sockets: listening on named pipe %q: %w", path, err)
	}
	return l, nil
}
