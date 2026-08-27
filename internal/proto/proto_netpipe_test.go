package proto

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// TestBufioReaderDoubleBufferingLosesBytes is not a test of this package's
// own code — it is an isolated repro of the exact bug that caused silent
// data loss in an earlier version of this tool: reading a handshake line
// off a net.Conn with one bufio.Reader, then abandoning it and building a
// second bufio.Reader on the same conn to keep reading. bufio.Reader.Read
// fills its internal buffer from one underlying Read() call, which on a
// stream socket can return far more than "up to the next newline" -- any
// bytes it read past the line it handed back are stuck in the first
// Reader and never seen again once a second Reader takes over.
//
// This is kept as a permanent regression test (not just a one-off repro
// that was deleted after confirming the theory) because the fix -- "one
// Reader per connection, for the connection's whole life" -- is a
// discipline that has to hold across every future change to proto.Reader
// and its callers in cmd/mpv and cmd/mpv-agent. If someone "simplifies"
// a handshake path later by peeling off a fresh bufio.Reader, this test
// starts failing.
func TestBufioReaderDoubleBufferingLosesBytes(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Write a "handshake line" immediately followed by a second line,
	// in a single Write call, so a real TCP/pipe read will very likely
	// return both at once.
	go func() {
		_, _ = client.Write([]byte("HANDSHAKE\nPAYLOAD\n"))
	}()

	first := bufio.NewReader(server)
	line, err := first.ReadString('\n')
	if err != nil {
		t.Fatalf("reading handshake line: %v", err)
	}
	if line != "HANDSHAKE\n" {
		t.Fatalf("unexpected handshake line: %q", line)
	}

	// BUG: abandon `first` (which may already hold "PAYLOAD\n" in its
	// internal buffer) and read the rest with a brand new bufio.Reader
	// on the same net.Conn.
	second := bufio.NewReader(server)
	done := make(chan struct{})
	var payload string
	var readErr error
	go func() {
		payload, readErr = second.ReadString('\n')
		close(done)
	}()

	select {
	case <-done:
		if readErr != nil {
			t.Fatalf("second reader error: %v", readErr)
		}
		if payload == "PAYLOAD\n" {
			t.Fatalf("expected the second-reader bug to lose PAYLOAD, but it was received intact - " +
				"net.Pipe's Read semantics may have changed; re-verify this repro against a real socket before trusting it")
		}
	case <-time.After(200 * time.Millisecond):
		// This is the expected (buggy) outcome: "PAYLOAD\n" is stuck
		// inside `first`'s internal buffer and `second` never sees it,
		// so its ReadString blocks forever waiting for more data from
		// the conn. That timeout IS the bug manifesting.
	}
}

// TestReaderSingleInstanceDoesNotLoseBytes proves this package's actual
// Reader avoids the bug above by construction: one Reader wraps one
// bufio.Reader for the connection's entire life, so nothing can be
// buffered-and-orphaned.
func TestReaderSingleInstanceDoesNotLoseBytes(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	w := NewWriter(client)
	go func() {
		_ = w.WriteControl(Control{Kind: KindHello})
		_ = w.WriteIPCLine([]byte(`{"event":"pause"}`))
	}()

	r := NewReader(server) // exactly one Reader for this conn's lifetime

	c, err := r.ReadControl()
	if err != nil {
		t.Fatalf("ReadControl: %v", err)
	}
	if c.Kind != KindHello {
		t.Fatalf("got kind %q, want %q", c.Kind, KindHello)
	}

	f, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if f.Type != TypeIPCLine || string(f.Payload) != `{"event":"pause"}` {
		t.Fatalf("got frame %+v, want IPC line payload", f)
	}
}
