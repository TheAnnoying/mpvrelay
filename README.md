# mpvrelay

Lets a headless Seanime server (no display — e.g. a NAS running Seanime
in Docker) keep using its normal "launch mpv, track playback over its
JSON-IPC socket" feature, while the video actually plays with a real mpv
on a separate desktop machine on the same LAN. Either machine can be
Linux or Windows — both binaries build for both.

Two binaries:

- **`cmd/mpv`** — built as a binary literally named `mpv` (or `mpv.exe`)
  and placed where Seanime's server looks for its media player. Seanime
  spawns it exactly like real mpv. It creates the IPC socket/pipe itself
  and relays traffic to/from the client agent over TCP.
- **`cmd/mpv-agent`** — runs persistently on the client machine, dials
  the server, and on each session launches real `mpv` there, tunneling
  its actual IPC socket/pipe back over the same connection.

Everything in between is relayed byte-for-byte. The only code that ever
looks inside an IPC line lives in `internal/rewrite`, and it does exactly
two things (see that package's doc comment for the full reasoning):
rewrite a `loadfile`'s real filesystem path into a URL the client machine
can fetch, and rewrite the `path`/`filename` properties reported back so
Seanime's local-file matching (which requires an exact string match —
see `getLocalFilePlaybackDetails` in Seanime's own source) still finds
the file in its library.

It reuses Seanime's own `/api/v1/mediastream/file` endpoint to serve the
actual file bytes (Range-request support, library-path validation and
HMAC-token auth all already implemented there) instead of running a
bespoke file server — see `internal/rewrite/stream_url.go`.

## Requirements this was built against

- A Seanime server whose media library is not reachable from the client
  machine at all (no share, no mapped drive) — the video is streamed
  over HTTP from Seanime itself.
- A single playback session at a time (personal/homelab use, not
  multi-user).
- The server reachable from the client machine via a LAN hostname
  (mDNS-style, e.g. `myserver.local`) and/or a published Docker port.

## How it fits together

```
Seanime (server) --socket/pipe--> [mpv stub] --TCP--> [mpv-agent] --socket/pipe--> real mpv (client)
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
5. The agent starts real `mpv` with a fresh local socket/pipe, dials it,
   and from then on relays IPC lines in both directions. The stub
   rewrites `loadfile` commands going one way and `path`/`filename`
   property-changes coming back the other way; nothing else is touched.
6. The session ends when Seanime closes its connection or real mpv exits
   on the client side; the agent kills mpv (if it's still around) and
   goes back to redialing.

Because Seanime reuses the *same* mpv process (and IPC connection) for
"next episode" via `loadfile ... replace` instead of restarting it, the
same is true here for free: within one continuous watch session, only
the *first* episode pays for a cold `mpv` start. This is why the
system doesn't try to keep a pre-warmed mpv instance around across
*separate* sessions — do that only if the timing logs described below
actually show it's needed; nothing here has needed it in testing.

## Building

Both binaries build for either Linux or Windows — the stub runs wherever
Seanime runs, and the agent runs wherever the real, screen-having mpv is;
either one can be either OS.

```sh
cd tools/mpvrelay

# server-side stub — build for whatever Seanime's own host runs on
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o dist/mpv       ./cmd/mpv
GOOS=windows GOARCH=amd64 go build -o dist/mpv.exe   ./cmd/mpv

# client-side agent — build for whatever the real-mpv machine runs on
GOOS=windows GOARCH=amd64 go build -o dist/mpv-agent.exe ./cmd/mpv-agent
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o dist/mpv-agent     ./cmd/mpv-agent
```

Only build the pair that matches your actual setup — you don't need all
four.

There are no automated tests. Every reader (`bufio.Reader`/`bufio.Scanner`)
in this codebase must be created exactly once per connection and reused
for that connection's whole life — building a second one on a connection
that already had one silently drops whatever the first had buffered past
the point it stopped reading at. This caused real data loss earlier in
development; keep it in mind when touching connection-handling code.

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
| `SEANIME_MPV_RELAY_SEANIME_URL` | Seanime's own LAN-reachable base URL, e.g. `http://myserver.local:43211` — must be reachable **from the machine running the agent**, not just from inside the container | *(required)* |
| `SEANIME_MPV_RELAY_PASSWORD` | Seanime's server password (plaintext), if one is set. **Yes, the stub genuinely needs this** — not to authenticate the relay itself, but to mint the same HMAC token Seanime's own `/api/v1/mediastream/file` endpoint validates, so the agent's real mpv is allowed to fetch the video over HTTP. Leave unset only if Seanime has no server password configured. | *(empty)* |
| `SEANIME_MPV_RELAY_PORT` | TCP port to listen on for the agent | `43219` |

**The Docker container must publish `SEANIME_MPV_RELAY_PORT`** (e.g.
`-p 43219:43219`) so the agent can reach it — this is a separate port
from Seanime's own web/API port.

**Set a server password in Seanime's settings.** Without one, whether
`/api/v1/mediastream/file` accepts a request from the client's LAN
address depends on Seanime's security mode; with one, the HMAC token
this tool mints is unconditionally sufficient, independent of security
mode. This is also just good practice for anything reachable off the
server's own host.

## Deploying the client-side agent

Run the agent persistently on the machine with the real screen and real
mpv (a scheduled task at logon on Windows, a systemd user service on
Linux, or any equivalent of "start this in the background and keep it
running" works). It takes command-line flags:

| Flag | Meaning | Default |
|---|---|---|
| `-server` | `host:port` of the server's relay port, e.g. `myserver.local:43219` | *(required)* |
| `-mpv` | Path to the real `mpv`/`mpv.exe` | `mpv` (via `PATH`) |

```sh
mpv-agent -server myserver.local:43219
mpv-agent -server myserver.local:43219 -mpv "C:\Program Files\mpv\mpv.exe"
```

Everything else (retry interval, dial/handshake/pipe timeouts) is a fixed
constant in the code, not something a deployment should need to tune.

The server-side stub (`cmd/mpv`) still takes its configuration from
environment variables — see "Deploying the server-side stub" above,
including the cmd.exe quoting pitfall that applies there.

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
`mpv` startup, first IPC round trip) actually ate the budget — rather
than guessing.

## Known limitations (by design, not oversights)

- If the agent's TCP connection drops mid-session (network hiccup, the
  client machine sleeping), the session ends — there's no reconnect
  logic that resumes into an already-running `mpv`. For a single-session
  personal setup this is a deliberate simplicity trade-off, not a
  missing feature.
- The stub accepts exactly one Seanime connection and one agent
  connection and exits once that session ends, mirroring how Seanime
  itself only spawns one `mpv` process per non-reused session; it does
  not accept a second Seanime connection into an already-finished
  process.
- No cross-session mpv pre-warming (see "How it fits together" above
  for why).
