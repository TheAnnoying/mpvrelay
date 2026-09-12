//go:build windows

package sockets

import (
	"fmt"
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
)

func Dial(path string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(path, &timeout)
}

func Listen(path string) (net.Listener, error) {
	l, err := winio.ListenPipe(path, nil)
	if err != nil {
		return nil, fmt.Errorf("sockets: listening on named pipe %q: %w", path, err)
	}
	return l, nil
}
