// Package ipcline reads mpv's JSON-IPC wire format: newline-delimited
// JSON values, one per line, on either a real mpv IPC socket/pipe
// (internal/mediaplayers/mpvipc/mpvipc.go's Connection.listen uses
// bufio.Scanner for exactly this) or - equivalently, since it's the same
// format - the local named pipe cmd/mpv-agent opens to its own real mpv
// on the client.
package ipcline

import (
	"bufio"
	"bytes"
	"io"
)

// Reader reads one JSON-IPC line at a time. Exactly one Reader must be
// created per connection and reused for that connection's entire
// lifetime: see internal/proto's package doc for why a second
// bufio-backed reader on the same net.Conn silently drops data.
type Reader struct {
	r *bufio.Reader
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64*1024)}
}

// ReadLine returns the next line with its trailing "\n" (and any
// preceding "\r") stripped. A partial trailing line with no newline
// right before the connection closes is discarded along with the error
// that caused the read to fail - a clean IPC session never ends mid-line.
func (lr *Reader) ReadLine() ([]byte, error) {
	line, err := lr.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return bytes.TrimRight(line, "\r\n"), nil
}
