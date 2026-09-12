// Package proto is the one-time JSON handshake sent over the TCP
// connection between the "mpv" stub and the agent, before that same
// connection becomes a plain newline-delimited relay of raw mpv IPC
// lines (read with a bufio.Scanner directly in cmd/mpv/cmd/mpv-agent,
// same as the local socket/pipe side). No framing is needed: the
// handshake is always exactly one line each way, sent before any IPC
// line flows, so order alone disambiguates it.
package proto

// Launch is the one line the stub sends the agent right after it
// connects: what to open, and any extra mpv flags to forward.
type Launch struct {
	URL  string   `json:"url,omitempty"`
	Args []string `json:"args,omitempty"`
}

// Ack is the one line the agent sends back: an empty Error means its
// mpv is up and ready to relay; a non-empty Error means startup failed
// and the session should be torn down.
type Ack struct {
	Error string `json:"error,omitempty"`
}
