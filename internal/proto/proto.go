// Package proto frames the single TCP connection between the "mpv" stub
// and the agent: [1 byte type][4 bytes big-endian length][payload].
// TypeControl carries a JSON Control handshake message; TypeIPCLine
// carries one raw mpv JSON-IPC line, without its trailing newline.
//
// Exactly one Reader must be constructed per net.Conn and reused for
// that connection's whole life - bufio.Reader buffers ahead of frame
// boundaries, so a second Reader on the same conn silently drops
// whatever bytes the first had already buffered.
package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type FrameType byte

const (
	TypeControl FrameType = 0x01
	TypeIPCLine FrameType = 0x02
)

// MaxFrameLen bounds a single frame's payload, catching a desynced stream
// quickly instead of allocating unbounded memory for it.
const MaxFrameLen = 16 << 20 // 16 MiB

// Kind enumerates Control.Kind values.
type Kind string

const (
	KindHello  Kind = "hello"  // agent -> stub, first message after dialing
	KindLaunch Kind = "launch" // stub -> agent, "start (or reuse) real mpv with this URL/args"
	KindReady  Kind = "ready"  // agent -> stub, real mpv's IPC pipe is dialed and relaying
	KindError  Kind = "error"  // either direction, human-readable diagnostic; session should be torn down
	KindBye    Kind = "bye"    // either direction, graceful "the session is over"
)

// Control is the JSON payload of a TypeControl frame.
type Control struct {
	Kind Kind `json:"kind"`

	// URL is the mediastream URL for the file to load (KindLaunch only).
	URL string `json:"url,omitempty"`

	// Args are extra mpv CLI flags to forward verbatim (KindLaunch only).
	Args []string `json:"args,omitempty"`

	// Message is a human-readable detail for KindError/KindBye.
	Message string `json:"message,omitempty"`
}

// Writer serializes frames onto a connection. Safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

func (fw *Writer) writeFrame(typ FrameType, payload []byte) error {
	if len(payload) > MaxFrameLen {
		return fmt.Errorf("proto: frame too large (%d bytes)", len(payload))
	}
	header := make([]byte, 5)
	header[0] = byte(typ)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))

	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fw.w.Write(header); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := fw.w.Write(payload)
	return err
}

// WriteControl sends a Control message.
func (fw *Writer) WriteControl(c Control) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return fw.writeFrame(TypeControl, payload)
}

// WriteIPCLine sends one raw IPC line (without its trailing newline).
func (fw *Writer) WriteIPCLine(line []byte) error {
	return fw.writeFrame(TypeIPCLine, line)
}

// Frame is a decoded frame as returned by Reader.ReadFrame.
type Frame struct {
	Type    FrameType
	Payload []byte // for TypeControl, the raw JSON; for TypeIPCLine, the raw line
}

// Reader deserializes frames from a connection (see package doc: exactly
// one per connection, reused for its whole lifetime).
type Reader struct {
	r *bufio.Reader
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64*1024)}
}

func (fr *Reader) ReadFrame() (Frame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(fr.r, header); err != nil {
		return Frame{}, err
	}
	typ := FrameType(header[0])
	length := binary.BigEndian.Uint32(header[1:])
	if length > MaxFrameLen {
		return Frame{}, fmt.Errorf("proto: peer sent oversized frame (%d bytes)", length)
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(fr.r, payload); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Type: typ, Payload: payload}, nil
}

// ReadControl reads one frame and decodes it as a Control message. It
// returns an error if the frame was actually a TypeIPCLine frame -
// during the handshake phase the protocol never interleaves the two, so
// this indicates a desynced stream.
func (fr *Reader) ReadControl() (Control, error) {
	f, err := fr.ReadFrame()
	if err != nil {
		return Control{}, err
	}
	if f.Type != TypeControl {
		return Control{}, fmt.Errorf("proto: expected control frame, got type %d", f.Type)
	}
	var c Control
	if err := json.Unmarshal(f.Payload, &c); err != nil {
		return Control{}, fmt.Errorf("proto: invalid control payload: %w", err)
	}
	return c, nil
}
