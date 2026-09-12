# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

mpvrelay lets a headless Seanime server (e.g. a NAS running Seanime in
Docker, no display) keep using its normal "launch mpv, track playback over
its JSON-IPC socket" feature, while video actually plays with a real mpv on
a separate desktop machine on the same LAN. It ships as two Go binaries:

- **`cmd/mpv`** — the server-side stub, built as a binary literally named
  `mpv`/`mpv.exe` and placed where Seanime looks for its media player.
  Seanime spawns it exactly like real mpv; it creates the IPC socket/pipe
  itself and relays traffic to/from the client agent over TCP.
- **`cmd/mpv-agent`** — runs persistently on the client machine, dials the
  server, and on each session launches real `mpv` there, tunneling its
  actual IPC socket/pipe back over the same TCP connection.

Full design rationale, env vars, deployment steps and known limitations are
in [README.md](README.md) — read it before making non-trivial changes;
much of the "why" documented there is not repeated here.

## Commands

```sh
go build ./...          # compile everything for the host OS/arch
go vet ./...

# cross-compiling both binaries for a real deployment (see README for which pair you need)
GOOS=linux   GOARCH=amd64 go build -o dist/mpv           ./cmd/mpv
GOOS=windows GOARCH=amd64 go build -o dist/mpv.exe       ./cmd/mpv
GOOS=windows GOARCH=amd64 go build -o dist/mpv-agent.exe ./cmd/mpv-agent
GOOS=linux   GOARCH=amd64 go build -o dist/mpv-agent     ./cmd/mpv-agent
```

There is no lint config beyond `go vet`; there's no CI and no test suite in this repo.

## Architecture

```
Seanime (server) --socket/pipe--> [mpv stub] --TCP--> [mpv-agent] --socket/pipe--> real mpv (client)
                                                                                          |
                                HTTP GET /api/v1/mediastream/file (Range) <---------------+
```

The two binaries share a single TCP connection with no binary framing at
all: `internal/proto` defines just two tiny JSON structs, `Launch`
(stub→agent, sent once right after connecting: the mediastream URL plus
any extra mpv flags) and `Ack` (agent→stub, sent once: empty `Error`
means its mpv is up and relaying is starting). Each is exactly one
newline-terminated JSON line; order alone disambiguates the handshake
from what follows, since it always happens first. After that one
exchange, the *same* connection becomes a plain newline-delimited relay
of raw mpv IPC lines — read with a `bufio.Scanner` directly in
`cmd/mpv`/`cmd/mpv-agent`, the identical pattern already used on the
local socket/pipe side. There's no mid-session control channel: a
session ends when either side closes the connection, and each side logs
its own reason locally rather than the peer's.

**Byte-for-byte relay is the default; content-aware rewriting is the
narrow exception.** `internal/rewrite` is the *only* package in the whole
system that ever parses an IPC line's JSON. It does exactly two things,
both required because the client machine has no filesystem access to the
server's media library:

1. `RewriteOutgoing` — server → client: rewrites a `loadfile`'s real
   absolute path into a Seanime `/api/v1/mediastream/file` URL
   (`stream_url.go`'s `BuildStreamURL`, which mints Seanime's own HMAC
   auth token from just the plaintext server password, entirely outside
   the Seanime process — see `internal/rewrite/stream_url.go`'s doc
   comment for the exact scheme it mirrors).
2. `RewriteIncoming` — client → server: rewrites `path`/`filename`
   property-change events reporting that URL back into the original real
   path/filename, because Seanime's local-file matching requires an exact
   string match on `path`. The reverse mapping is stateless by design —
   the real path is encoded in the stream URL's own query string (see
   `ExtractPathFromStreamURL`), not tracked as mutable session state, so
   it can't desync when two `loadfile`s are in flight at once (e.g.
   pre-queuing the next episode).

Every other package treats IPC content as opaque bytes.

**Session lifecycle** (see `cmd/mpv/main.go`'s `run` and
`cmd/mpv-agent/main.go`'s `runSession`): the stub listens on both the IPC
socket (for Seanime) and a fixed TCP port (for the agent) and accepts
whichever connects first (`acceptBoth`); the agent is expected to already
be running and redialing that fixed port on a short interval *before* any
session starts, since there's no earlier signal than the stub process
itself being spawned. One TCP connection = one playback session; the stub
exits after the session ends rather than accepting a second Seanime
connection.

**Critical invariant, no longer test-enforced:** exactly one
`bufio.Scanner` must be constructed per connection and reused for that
connection's entire life — including across the handshake and the relay
loop that follows on the same connection (see `cmd/mpv`'s `agentScanner`
and `cmd/mpv-agent`'s `scanner` in `runSession`, each created once and
handed into the goroutine that keeps reading). A second reader built on
the same `net.Conn` silently drops whatever bytes the first had already
buffered past its last read — this caused real data loss in earlier
development and has no regression test protecting it anymore, so treat
it as a hard rule when touching connection-handling code, not just a
suggestion. The corresponding write side needs no such care and no
mutex: each connection has exactly one writer at a time by construction
(the handshake line is written before any relay goroutine starts; after
that, only one goroutine ever writes to a given connection).

**Platform split**: `internal/sockets` has unix/windows build-tagged
files (`unix.go`/`windows.go`) — Unix domain socket vs. Windows named
pipe — behind one shared `Dial`/`Listen` API. Both binaries build for
either OS; which one runs where depends on the deployment (stub on
Seanime's host, agent on the real-mpv machine), not on the code.

**Config**: the two binaries take configuration differently. `cmd/mpv`
(the server stub) reads env vars only, through its own private `getenv`
helper (`cmd/mpv/main.go`), which strips a `cmd.exe`-style stray pair of
quotes (`set VAR="value"` leaves the quotes in Go's `os.Getenv`) — any
new server env var should go through this same helper. `cmd/mpv-agent`
takes command-line flags (`-server`, `-mpv`) via the stdlib `flag`
package instead, and has no env-var config at all.

**Logging**: `internal/rlog.Printf` writes timestamped lines to stderr
only (stdout is reserved on the stub side for the startup line Seanime's
launcher scans for). Every state transition should be logged this way —
the whole point is lining up the stub's and agent's logs to diagnose which
step in the timing budget (agent redial latency, real mpv startup, first
IPC round trip) ate the time, per the README's "Diagnosing startup timing"
section.
