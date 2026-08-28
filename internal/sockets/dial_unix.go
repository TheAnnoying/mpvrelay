//go:build !windows

package sockets

import (
	"net"
	"time"
)

// Dial connects to a local Unix domain socket, e.g. the one real mpv
// creates for --input-ipc-server on Linux/macOS. Mirrors dial_windows.go's
// named-pipe dial for the same role on Windows.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}
