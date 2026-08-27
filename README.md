# mpvrelay

Lets a headless Seanime server (no display — e.g. a Linux box/NAS running
Seanime in Docker) keep using its normal "launch mpv, track playback over
its JSON-IPC socket" feature, while the video actually plays with a real
mpv on a separate Windows desktop machine on the same LAN.

Two binaries:

- **`cmd/mpv`** — built as a binary literally named `mpv` and placed
  where Seanime's server looks for its media player. Seanime spawns it
  exactly like real mpv. It creates the IPC socket itself and relays
  traffic to/from the client agent over TCP.
- **`cmd/mpv-agent`** — runs persistently on the Windows machine, dials
  the server, and on each session launches real `mpv.exe` there,
  tunneling its actual IPC pipe back over the same connection.

Everything in between is relayed byte-for-byte. The only code that ever
looks inside an IPC line lives in `internal/rewrite`, and it does exactly
two things (see that package's doc comment for the full reasoning):
rewrite a `loadfile`'s real filesystem path into a URL the Windows
machine can fetch, and rewrite the `path`/`filename` properties reported
back so Seanime's local-file matching (which requires an exact string
match — see `getLocalFilePlaybackDetails` in Seanime's own source) still
finds the file in its library.

It reuses Seanime's own `/api/v1/mediastream/file` endpoint to serve the
actual file bytes (Range-request support, library-path validation and
HMAC-token auth all already implemented there) instead of running a
bespoke file server — see `internal/rewrite/stream_url.go`.

## Requirements this was built against

- A Seanime server whose media library is not reachable from the Windows
  machine at all (no share, no mapped drive) — the video is streamed
  over HTTP from Seanime itself.
- A single playback session at a time (personal/homelab use, not
  multi-user).
- The server reachable from the Windows machine via a LAN hostname
  (mDNS-style, e.g. `myserver.local`) and/or a published Docker port.

## How it fits together

```
Seanime (Linux/Docker) --unix socket--> [mpv stub] --TCP--> [mpv-agent] --named pipe--> real mpv.exe (Windows)
                                                                                              |
                                    HTTP GET /api/v1/mediastream/file (Range) <---------------+
```

1. Seanime spawns `mpv --input-ipc-server=<sock> ... <realAbsolutePath>`.
2. The stub creates `<sock>` itself and starts listening on a fixed TCP
   port (`SEANIME_MPV_RELAY_PORT`, default `43219`) for the agent.
3. The agent is expected to already be running, redialing that TCP port
   on a short interval — there's no way to signal "a session is about to
   start" any earlier than the stub process actually being spawned
   without patching Seanime itself, so the agent has to already be
   waiting when that happens. Because the stub always listens on the
   *same* fixed port, the agent's very next retry after the stub comes
   up succeeds within one retry interval, not "however long a fresh
   process takes to launch."
4. Once both sides are connected, the stub builds a mediastream URL for
   the real path, mints an HMAC token for it (reproducing Seanime's own
   token scheme — see `internal/rewrite/stream_url.go` — entirely
   outside the Seanime process, from just the plaintext server
   password), and sends the agent a `launch` message with that URL plus
   any passthrough mpv flags.
5. The agent starts `mpv.exe` with a fresh local named pipe, dials it,
   and from then on relays IPC lines in both directions. The stub
   rewrites `loadfile` commands going one way and `path`/`filename`
   property-changes coming back the other way; nothing else is touched.
6. The session ends when Seanime closes its connection or real mpv exits
   on the Windows side; the agent kills mpv.exe (if it's still around)
   and goes back to redialing.

Because Seanime reuses the *same* mpv process (and IPC connection) for
"next episode" via `loadfile ... replace` instead of restarting it, the
same is true here for free: within one continuous watch session, only
the *first* episode pays for a cold `mpv.exe` start. This is why the
system doesn't try to keep a pre-warmed mpv instance around across
*separate* sessions — do that only if the timing logs described below
actually show it's needed; nothing here has needed it in testing.

## Building

```sh
cd tools/mpvrelay

# server-side stub — build for whatever the Seanime container runs on
GOOS=linux GOARCH=amd64 go build -o dist/mpv ./cmd/mpv

# client-side agent — Windows only (it dials named pipes, uses go-winio)
GOOS=windows GOARCH=amd64 go build -o dist/mpv-agent.exe ./cmd/mpv-agent
```

Run `go test ./...` from this directory to run the unit tests, including
an isolated `net.Pipe()` regression test for a `bufio.Reader` pitfall
that caused real data loss during development (see
`internal/proto/proto_netpipe_test.go`): building a second `bufio.Reader`
on a connection that already had one silently drops whatever the first
reader had buffered past the point it stopped reading at. Every reader in
this codebase is created exactly once per connection and reused for that
connection's whole life — that test exists to keep it that way.

## Deploying the server-side stub

Put the built `mpv` binary somewhere in the Seanime container and either:

- name it `mpv` and put its directory earlier in `PATH` than any real
  `mpv`, or (recommended, since it doesn't risk shadowing a real `mpv`
  something else on the system might want)
- point Seanime's media player settings at its absolute path directly.

It needs these environment variables (available to the Seanime
container/process, e.g. via `docker-compose`'s `environment:`):

| Variable | Meaning | Default |
|---|---|---|
| `SEANIME_MPV_RELAY_PORT` | TCP port to listen on for the agent | `43219` |
| `SEANIME_MPV_RELAY_SEANIME_URL` | Seanime's own LAN-reachable base URL, e.g. `http://myserver.local:43211` — must be reachable **from the Windows machine**, not just from inside the container | *(required)* |
| `SEANIME_MPV_RELAY_PASSWORD` | Seanime's server password (plaintext) — used only locally to mint the same HMAC token Seanime itself would validate. Leave unset if the server has no password configured. | *(empty)* |

**The Docker container must publish `SEANIME_MPV_RELAY_PORT`** (e.g.
`-p 43219:43219`) so the Windows agent can reach it — this is a separate
port from Seanime's own web/API port.

**Set a server password in Seanime's settings.** Without one, whether
`/api/v1/mediastream/file` accepts a request from the Windows machine's
LAN address depends on Seanime's security mode; with one, the HMAC token
this tool mints is unconditionally sufficient, independent of security
mode. This is also just good practice for anything reachable off the
server's own host.

## Deploying the client-side agent

Run `mpv-agent.exe` persistently on the Windows machine (a scheduled
task at logon, or any equivalent of "start this in the background and
keep it running" works). It needs:

| Variable | Meaning | Default |
|---|---|---|
| `SEANIME_MPV_RELAY_SERVER` | `host:port` of the server's relay port, e.g. `myserver.local:43219` | *(required)* |
| `SEANIME_MPV_RELAY_MPV_PATH` | Path to the real `mpv.exe` | `mpv` (via `PATH`) |
| `SEANIME_MPV_RELAY_RETRY_INTERVAL` | How often to retry dialing the server | `500ms` |
| `SEANIME_MPV_RELAY_DIAL_TIMEOUT` | Per-attempt TCP dial timeout | `3s` |
| `SEANIME_MPV_RELAY_PIPE_TIMEOUT` | How long to wait for `mpv.exe`'s IPC pipe after starting it | `10s` |
| `SEANIME_MPV_RELAY_HANDSHAKE_TIMEOUT` | How long to wait for the stub's `hello`/`launch` messages after connecting, before giving up and redialing | `10s` |

**cmd.exe quoting pitfall:** `set VAR="value"` does not strip the quotes
the way a POSIX shell would — `os.Getenv` would see the literal
characters `"value"`, quotes included, if this weren't handled. Every
value read from the environment on both sides goes through
`internal/envcfg.Clean`, which trims whitespace and a matching pair of
leading/trailing quote characters, so configuring things this way (or via
a GUI that does the same thing) doesn't silently break in a confusing
way.

## Diagnosing startup timing

Seanime gives up waiting for the first player status update fairly
quickly if the media player doesn't report one (~3s before the first
check, then a handful of 1s-spaced retries — see
`internal/mediaplayers/mediaplayer/repository.go`'s `StartTracking`, and
its default `MediaPlayerLocalFileTrackingRequestedEvent` values). Both
binaries log every state transition to stderr with a wall-clock
timestamp and a "+Nms since process start" offset
(`internal/rlog`), specifically so that if a session ever fails with
"Failed to get player status" in Seanime's own logs, you can line the two
binaries' logs up and see exactly which step (agent redial latency, real
`mpv.exe` startup, first IPC round trip) actually ate the budget — rather
than guessing.

## Known limitations (by design, not oversights)

- If the agent's TCP connection drops mid-session (network hiccup,
  Windows machine sleeping), the session ends — there's no reconnect
  logic that resumes into an already-running `mpv.exe`. For a
  single-session personal setup this is a deliberate simplicity
  trade-off, not a missing feature.
- The stub accepts exactly one Seanime connection and one agent
  connection and exits once that session ends, mirroring how Seanime
  itself only spawns one `mpv` process per non-reused session; it does
  not accept a second Seanime connection into an already-finished
  process.
- No cross-session mpv.exe pre-warming (see "How it fits together"
  above for why).
