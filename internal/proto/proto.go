// Package proto implements the single-TCP-connection framing used between
// the server-side "mpv" stub and the client-side agent.
//
// Every frame is:
//
//	[1 byte type][4 bytes big-endian length][length bytes of payload]
//
// Two frame types share the connection:
//
//   - TypeControl: a JSON-encoded Control message (session handshake:
//     "launch a player", "player is ready", "player exited", ...).
//   - TypeIPCLine: the raw bytes of exactly one line of mpv's JSON-IPC
//     protocol (i.e. one JSON value), without the trailing newline.
//     Framing the line explicitly means neither side needs to re-scan for
//     newlines downstream, and it sidesteps having to reimplement mpv's
//     line-delimited protocol as a byte-stream: each frame is forwarded
//     as an atomic, already-delimited unit.
//
// A single connection carries one playback session: the stub opens it
// once an agent dials in and a "launch" has been acknowledged with
// "ready", and either side tears it down when the session ends (real mpv
// exiting on the client, or Seanime closing its socket on the server).
//
// IMPORTANT (see package-level doc in cmd/mpv): callers must construct
// exactly one Reader per net.Conn and keep using it for that connection's
// entire lifetime. Reader wraps a bufio.Reader, and bufio.Reader reads
// whole chunks from the underlying conn, not just up to a frame boundary.
// Discarding a Reader and building a second one on the same net.Conn
// silently drops whatever the first one had already buffered past the
// last frame it returned. See proto_netpipe_test.go for a regression test
// that fails if this pattern is violated.
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

// MaxFrameLen bounds a single frame's payload. mpv IPC lines are always
// small (property values, short commands); this is generous headroom
// while still catching a desynced stream quickly instead of trying to
// allocate gigabytes for it.
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
	// Empty means "start mpv idle, no file yet" (mirrors mpv.go's unused
	// --idle launch path, kept here for completeness/robustness).
	URL string `json:"url,omitempty"`

	// Args are extra mpv CLI flags to forward verbatim (KindLaunch only).
	// The stub has already stripped --input-ipc-server, --log-file and
	// the trailing file path from the original argv — see cmd/mpv.
	Args []string `json:"args,omitempty"`

	// Message is a human-readable detail for KindError/KindBye.
	Message string `json:"message,omitempty"`
}

// Writer serializes frames onto a connection. It is safe for concurrent
// use by multiple goroutines (the relay loops in each direction write to
// the same connection independently).
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

// Reader deserializes frames from a connection. Exactly one Reader must
// be created per connection and reused for that connection's whole
// lifetime (see package doc).
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
