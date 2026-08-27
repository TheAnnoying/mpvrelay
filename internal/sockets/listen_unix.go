//go:build !windows

// Package sockets creates the listener side of whatever IPC transport
// mpv.go's --input-ipc-server value implies: a Unix domain socket
// everywhere real deployment happens (the stub always runs on the Linux
// server inside Seanime's Docker container - see
// internal/mediaplayers/mpvipc/pipe.go's dial("unix", path), which is
// what Seanime dials against), or a Windows named pipe when building and
// exercising cmd/mpv on a Windows dev machine for convenience (see
// pipe_windows.go in the same Seanime package).
package sockets

import (
	"fmt"
	"net"
	"os"
)

// Listen creates the listener Seanime will dial as the mpv IPC socket.
func Listen(path string) (net.Listener, error) {
	// mpv itself unlinks a stale socket path before binding (it's a
	// leftover from an unclean previous exit, not a live listener - a
	// live one would make bind fail with EADDRINUSE the normal way).
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("sockets: removing stale socket %q: %w", path, err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("sockets: listening on unix socket %q: %w", path, err)
	}
	return l, nil
}
