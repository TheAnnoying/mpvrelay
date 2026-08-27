// Command mpv-agent runs persistently on the Windows desktop machine
// that has a real screen and a real mpv.exe. It dials the server-side
// stub (cmd/mpv) over TCP, and on each playback session launches real
// mpv.exe locally and tunnels its actual JSON-IPC named pipe back over
// that same TCP connection - byte for byte, with zero interpretation of
// the traffic. All content-aware rewriting (see internal/rewrite) lives
// on the server side, which is the only side that knows the real library
// paths, the server's own base URL and its auth secret; this binary only
// ever deals in opaque IPC lines.
//
// It is meant to already be running - e.g. as a scheduled task or a
// background process started at login - and idly retrying its
// connection well before any playback session starts, so that once
// Seanime spawns cmd/mpv on the server and that binary starts listening,
// this agent's very next retry (a fraction of a second later, not a
// fresh process launch) is what completes the handshake. See
// ../../README.md for the exact env vars and the cmd.exe quoting pitfall
// they need to avoid.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"mpvrelay/internal/envcfg"
	"mpvrelay/internal/ipcline"
	"mpvrelay/internal/proto"
	"mpvrelay/internal/rlog"
	"mpvrelay/internal/sockets"
)

func main() {
	serverAddr := envcfg.Get(os.Getenv, "SEANIME_MPV_RELAY_SERVER", "")
	if serverAddr == "" {
		rlog.Printf("agent: fatal: SEANIME_MPV_RELAY_SERVER is not set (expected host:port, e.g. myserver.local:43219)")
		os.Exit(1)
	}
	mpvPath := envcfg.Get(os.Getenv, "SEANIME_MPV_RELAY_MPV_PATH", "mpv")
	retryInterval := durationEnv("SEANIME_MPV_RELAY_RETRY_INTERVAL", 500*time.Millisecond)
	dialTimeout := durationEnv("SEANIME_MPV_RELAY_DIAL_TIMEOUT", 3*time.Second)
	pipeReadyTimeout := durationEnv("SEANIME_MPV_RELAY_PIPE_TIMEOUT", 10*time.Second)
	handshakeTimeout := durationEnv("SEANIME_MPV_RELAY_HANDSHAKE_TIMEOUT", 10*time.Second)

	rlog.Printf("agent: starting; server=%s mpv=%s", serverAddr, mpvPath)

	for {
		conn, err := net.DialTimeout("tcp", serverAddr, dialTimeout)
		if err != nil {
			time.Sleep(retryInterval)
			continue
		}
		rlog.Printf("agent: connected to stub at %s", serverAddr)
		runSession(conn, mpvPath, handshakeTimeout, pipeReadyTimeout)
		rlog.Printf("agent: session ended, resuming retry loop")
	}
}

// runSession handles exactly one TCP connection to the stub: one
// handshake (hello, then launch) followed by relaying until either side
// disconnects or real mpv exits. It never returns an error - by design,
// every failure here just means "go back to redialing", which main's
// loop already does.
//
// The handshake reads carry a bounded deadline. A plain TCP connect can
// succeed (and this agent will log "connected") even when the connection
// never actually delivers data afterward - a stale Docker port-forward
// left over from a container restart, a NAT/firewall that lets the SYN
// through but blackholes the rest, or a stub that never gets around to
// writing anything. Without a deadline here, a hung connection wedges
// this agent forever: it sits inside this one runSession call with
// nothing left to log and never returns to main's retry loop, so every
// later Seanime playback attempt spins up a fresh stub that waits for an
// agent that, from the outside, looks alive but is actually stuck talking
// to a session that already died. The deadline is cleared before the
// relay loop below, which must tolerate arbitrarily long silence (e.g. a
// paused video).
func runSession(conn net.Conn, mpvPath string, handshakeTimeout, pipeReadyTimeout time.Duration) {
	defer conn.Close()

	// One Reader/Writer pair for this conn's entire life - reused across
	// the handshake and the relay loop that follows. See internal/proto's
	// package doc: a second reader on the same conn would silently drop
	// whatever the first had already buffered.
	r := proto.NewReader(conn)
	w := proto.NewWriter(conn)

	if err := w.WriteControl(proto.Control{Kind: proto.KindHello}); err != nil {
		rlog.Printf("agent: failed to send hello: %v", err)
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))

	hello, err := r.ReadControl()
	if err != nil || hello.Kind != proto.KindHello {
		rlog.Printf("agent: did not receive hello from stub within %s (got %+v, err=%v)", handshakeTimeout, hello, err)
		return
	}

	launch, err := r.ReadControl()
	if err != nil {
		rlog.Printf("agent: failed to read launch message within %s: %v", handshakeTimeout, err)
		return
	}
	if launch.Kind != proto.KindLaunch {
		rlog.Printf("agent: expected launch, got kind=%q", launch.Kind)
		return
	}
	rlog.Printf("agent: received launch (hasURL=%v, args=%v)", launch.URL != "", launch.Args)

	_ = conn.SetReadDeadline(time.Time{})

	pipeName := freshPipeName()
	cmd, err := startMpv(mpvPath, pipeName, launch)
	if err != nil {
		rlog.Printf("agent: failed to start mpv: %v", err)
		_ = w.WriteControl(proto.Control{Kind: proto.KindError, Message: fmt.Sprintf("starting mpv: %v", err)})
		return
	}
	rlog.Printf("agent: started mpv.exe (pid=%d), waiting for its IPC pipe", cmd.Process.Pid)

	mpvExited := make(chan error, 1)
	go func() { mpvExited <- cmd.Wait() }()

	pipeConn, err := dialPipeWithRetry(pipeName, pipeReadyTimeout, mpvExited)
	if err != nil {
		rlog.Printf("agent: failed to connect to mpv's IPC pipe: %v", err)
		_ = w.WriteControl(proto.Control{Kind: proto.KindError, Message: fmt.Sprintf("connecting to mpv IPC pipe: %v", err)})
		killMpv(cmd, mpvExited)
		return
	}
	defer pipeConn.Close()
	rlog.Printf("agent: connected to mpv's IPC pipe, relaying")

	if err := w.WriteControl(proto.Control{Kind: proto.KindReady}); err != nil {
		rlog.Printf("agent: failed to send ready: %v", err)
		killMpv(cmd, mpvExited)
		return
	}

	errCh := make(chan error, 3)
	go relayTCPToPipe(r, pipeConn, errCh)
	go relayPipeToTCP(pipeConn, w, errCh)
	go func() {
		err := <-mpvExited
		errCh <- fmt.Errorf("mpv.exe exited: %v", err)
	}()

	sessionErr := <-errCh
	rlog.Printf("agent: relay ended: %v", sessionErr)
	_ = w.WriteControl(proto.Control{Kind: proto.KindBye, Message: sessionErr.Error()})

	killMpv(cmd, mpvExited)
}

func startMpv(mpvPath, pipeName string, launch proto.Control) (*exec.Cmd, error) {
	args := append([]string{}, launch.Args...)
	args = append(args, "--input-ipc-server="+pipeName)
	if launch.URL != "" {
		args = append(args, launch.URL)
	} else {
		args = append(args, "--idle")
	}

	cmd := exec.Command(mpvPath, args...)
	// mpv's own stdout/stderr chatter isn't needed by anything here, but
	// draining it (instead of leaving it connected to nothing/inherited)
	// avoids mpv ever blocking on a full pipe buffer.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func freshPipeName() string {
	return fmt.Sprintf(`\\.\pipe\mpvrelay_%d_%d`, os.Getpid(), time.Now().UnixNano())
}

// dialPipeWithRetry mirrors the retry shape of
// internal/mediaplayers/mpv/mpv.go's establishConnection: mpv.exe needs
// a brief moment after Start() returns before its named pipe server is
// actually listening. It also gives up early if mpv.exe has already
// exited, rather than waiting out the full timeout.
func dialPipeWithRetry(pipeName string, timeout time.Duration, mpvExited <-chan error) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := sockets.Dial(pipeName, 1*time.Second)
		if err == nil {
			return conn, nil
		}
		select {
		case exitErr := <-mpvExited:
			return nil, fmt.Errorf("mpv.exe exited before its IPC pipe came up: %v", exitErr)
		default:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s dialing %s: %w", timeout, pipeName, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func relayTCPToPipe(r *proto.Reader, pipeConn net.Conn, errCh chan<- error) {
	for {
		f, err := r.ReadFrame()
		if err != nil {
			errCh <- fmt.Errorf("stub->mpv: reading from stub: %w", err)
			return
		}
		switch f.Type {
		case proto.TypeIPCLine:
			if _, err := pipeConn.Write(append(f.Payload, '\n')); err != nil {
				errCh <- fmt.Errorf("stub->mpv: writing to mpv pipe: %w", err)
				return
			}
		case proto.TypeControl:
			var c proto.Control
			if json.Unmarshal(f.Payload, &c) == nil {
				rlog.Printf("agent: control from stub mid-session: kind=%s message=%q", c.Kind, c.Message)
				if c.Kind == proto.KindBye || c.Kind == proto.KindError {
					errCh <- fmt.Errorf("stub ended the session: %s", c.Message)
					return
				}
			}
		}
	}
}

func relayPipeToTCP(pipeConn net.Conn, w *proto.Writer, errCh chan<- error) {
	lines := ipcline.NewReader(pipeConn)
	for {
		line, err := lines.ReadLine()
		if err != nil {
			errCh <- fmt.Errorf("mpv->stub: reading from mpv pipe: %w", err)
			return
		}
		if err := w.WriteIPCLine(line); err != nil {
			errCh <- fmt.Errorf("mpv->stub: writing to stub: %w", err)
			return
		}
	}
}

// killMpv makes sure mpv.exe is gone before this session's cleanup
// finishes, so the next session on this agent is guaranteed a clean
// start (see README - deliberately not attempting to keep a warm
// instance around across sessions yet).
func killMpv(cmd *exec.Cmd, mpvExited <-chan error) {
	select {
	case <-mpvExited:
		return // already exited on its own
	default:
	}
	// Ask mpv to quit itself first would require sending an IPC "quit"
	// command down a pipe that may already be gone; a direct kill is
	// simpler and this tool doesn't need mpv to save any state on exit.
	_ = cmd.Process.Kill()
	select {
	case <-mpvExited:
	case <-time.After(3 * time.Second):
		rlog.Printf("agent: mpv.exe did not exit within 3s of being killed")
	}
}

func durationEnv(key string, def time.Duration) time.Duration {
	raw := envcfg.Get(os.Getenv, key, "")
	if raw == "" {
		return def
	}
	if ms, convErr := strconv.Atoi(raw); convErr == nil {
		return time.Duration(ms) * time.Millisecond
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		rlog.Printf("agent: invalid duration %q for %s, using default %s", raw, key, def)
		return def
	}
	return d
}
