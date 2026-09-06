//go:build !windows

// Package sockets creates the listener/dialer side of mpv's
// --input-ipc-server transport: a Unix domain socket here, a Windows
// named pipe in windows.go.
package sockets

import (
	"fmt"
	"net"
	"os"
	"time"
)

// Dial connects to a local Unix domain socket, e.g. the one real mpv
// creates for --input-ipc-server.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

// Listen creates the listener Seanime will dial as the mpv IPC socket.
func Listen(path string) (net.Listener, error) {
	// A stale socket path is a leftover from an unclean exit, not a live
	// listener (a live one fails the bind below with EADDRINUSE).
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("sockets: removing stale socket %q: %w", path, err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("sockets: listening on unix socket %q: %w", path, err)
	}
	return l, nil
}
