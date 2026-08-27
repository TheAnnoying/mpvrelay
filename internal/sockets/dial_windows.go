//go:build windows

package sockets

import (
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// Dial connects to a local named pipe, e.g. the one the agent just told
// its freshly-spawned real mpv.exe to create via --input-ipc-server.
// Mirrors internal/mediaplayers/mpvipc/pipe_windows.go's dial exactly
// (same timeout), since this is dialing the same kind of pipe real mpv
// creates for Seanime, just addressed to a local mpv.exe instead.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(path, &timeout)
}
