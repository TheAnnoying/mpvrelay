//go:build windows

package sockets

import (
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
)

// Listen creates the listener Seanime will dial as the mpv IPC socket,
// for when the stub runs on a Windows Seanime host (see listen_unix.go
// for the Linux/macOS equivalent).
func Listen(path string) (net.Listener, error) {
	l, err := winio.ListenPipe(path, nil)
	if err != nil {
		return nil, fmt.Errorf("sockets: listening on named pipe %q: %w", path, err)
	}
	return l, nil
}
