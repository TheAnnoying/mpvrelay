// Command mpv is a drop-in replacement for the real mpv binary, meant to
// be placed on the PATH of (or configured as the media player path in)
// a headless Seanime server - typically inside the same Docker container
// Seanime itself runs in, on Linux or Windows.
//
// Seanime (internal/mediaplayers/mpv/mpv.go's launchPlayer/createCmd)
// spawns this binary with an argv shaped like:
//
//	mpv [user-args...] --input-ipc-server=<socket-or-pipe-path> [--log-file=<path>] <real-absolute-file-path>
//
// and then dials that path (a Unix domain socket on Linux, a named pipe on
// Windows - see internal/mediaplayers/mpvipc) to speak mpv's JSON-IPC
// protocol over it. This binary:
//
//  1. Creates that socket/pipe itself and accepts Seanime's connection, so
//     Seanime's side of the integration is completely unmodified: as far
//     as it can tell, it dialed a real, running mpv.
//  2. Accepts a TCP connection from the client-side agent
//     (cmd/mpv-agent), running on whichever machine actually has a screen
//     and real mpv, and tells it what to play.
//  3. Relays every JSON-IPC line between the two, verbatim, except for
//     the narrow rewriting documented in internal/rewrite: the file path
//     in "loadfile" commands (server -> client) becomes a URL, and the
//     "path"/"filename" property-change events reporting that URL back
//     (client -> server) are translated back to the original real path.
//     No other command or property this doesn't specifically name is
//     ever inspected, let alone modified.
//
// See ../../README.md for deployment: PATH/settings wiring, required
// environment variables and the Docker port that needs publishing.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"mpvrelay/internal/envcfg"
	"mpvrelay/internal/ipcline"
	"mpvrelay/internal/proto"
	"mpvrelay/internal/rewrite"
	"mpvrelay/internal/rlog"
	"mpvrelay/internal/sockets"
)

// acceptTimeout and readyTimeout aren't configurable - they're not tuning
// knobs a user should ever need to reach for, just the fixed shape of
// "give up eventually instead of hanging forever."
const (
	acceptTimeout = 30 * time.Second
	readyTimeout  = 15 * time.Second
)

func main() {
	if err := run(); err != nil {
		rlog.Printf("stub: fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	ipcSocketPath, mediaPath, passthroughArgs := parseArgv(args)
	rlog.Printf("stub: launched with argv=%q", args)
	rlog.Printf("stub: ipcSocket=%q mediaPath=%q passthroughArgs=%q", ipcSocketPath, mediaPath, passthroughArgs)

	// Seanime's launchPlayer waits for up to ~2s for a non-"AV:" line of
	// stdout output before proceeding (and then, unconditionally, one
	// more full second) - see internal/mediaplayers/mpv/mpv.go. Emitting
	// this immediately, before doing anything else, keeps that wait as
	// short as mpv's own startup chatter would.
	fmt.Println("mpvrelay-stub: starting")

	if ipcSocketPath == "" {
		return errors.New("no --input-ipc-server=<path> argument in argv; " +
			"this binary is only meant to be launched by Seanime, not run by hand")
	}

	relayPort := envcfg.Get(os.Getenv, "SEANIME_MPV_RELAY_PORT", "43219")
	serverBaseURL := envcfg.Get(os.Getenv, "SEANIME_MPV_RELAY_SEANIME_URL", "")
	serverPassword := envcfg.Get(os.Getenv, "SEANIME_MPV_RELAY_PASSWORD", "")

	if mediaPath != "" && serverBaseURL == "" {
		return fmt.Errorf("SEANIME_MPV_RELAY_SEANIME_URL is not set; cannot build a stream URL for %q", mediaPath)
	}

	ipcListener, err := sockets.Listen(ipcSocketPath)
	if err != nil {
		return fmt.Errorf("listening on IPC socket: %w", err)
	}
	defer ipcListener.Close()
	rlog.Printf("stub: listening for Seanime on IPC socket %s", ipcSocketPath)

	tcpListener, err := net.Listen("tcp", ":"+relayPort)
	if err != nil {
		return fmt.Errorf("listening on relay TCP port %s: %w", relayPort, err)
	}
	defer tcpListener.Close()
	rlog.Printf("stub: listening for agent on TCP port %s", relayPort)

	seanimeConn, agentConn, err := acceptBoth(ipcListener, tcpListener, acceptTimeout)
	if err != nil {
		return err
	}
	defer seanimeConn.Close()
	defer agentConn.Close()

	// Exactly one Reader/Writer pair for agentConn's entire lifetime -
	// reused across the handshake below and the relay loop later. See
	// internal/proto's package doc for why a second Reader on the same
	// conn would silently drop buffered bytes.
	agentR := proto.NewReader(agentConn)
	agentW := proto.NewWriter(agentConn)

	if err := agentW.WriteControl(proto.Control{Kind: proto.KindHello}); err != nil {
		return fmt.Errorf("sending hello to agent: %w", err)
	}

	launch := proto.Control{Kind: proto.KindLaunch, Args: passthroughArgs}
	if mediaPath != "" {
		streamURL, err := rewrite.BuildStreamURL(serverBaseURL, serverPassword, mediaPath)
		if err != nil {
			return fmt.Errorf("building stream URL for %q: %w", mediaPath, err)
		}
		launch.URL = streamURL
		rlog.Printf("stub: built stream URL for real mpv to open")
	}
	rlog.Printf("stub: sending launch to agent (hasURL=%v, %d passthrough args)", launch.URL != "", len(launch.Args))
	if err := agentW.WriteControl(launch); err != nil {
		return fmt.Errorf("sending launch to agent: %w", err)
	}

	waitForReady(agentConn, agentR, readyTimeout)

	rewriter := rewrite.NewIPCRewriter(serverBaseURL, serverPassword)

	errCh := make(chan error, 2)
	go relaySeanimeToAgent(seanimeConn, agentW, rewriter, errCh)
	go relayAgentToSeanime(agentR, seanimeConn, rewriter, errCh)

	sessionErr := <-errCh
	rlog.Printf("stub: session ended: %v", sessionErr)

	_ = agentW.WriteControl(proto.Control{Kind: proto.KindBye, Message: sessionErr.Error()})

	return nil
}

// parseArgv splits Seanime's argv into the three things this binary
// cares about. createCmd (internal/mediaplayers/mpv/mpv.go) always
// appends the file path last, after every flag (its own, and any
// user-configured "Additional mpv arguments"), so "the last argument,
// if it isn't itself a flag" is a reliable way to identify it without
// having to enumerate every possible mpv flag shape.
func parseArgv(args []string) (ipcSocketPath, mediaPath string, passthrough []string) {
	n := len(args)
	lastIsPath := n > 0 && !strings.HasPrefix(args[n-1], "-")

	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--input-ipc-server="):
			ipcSocketPath = strings.TrimPrefix(a, "--input-ipc-server=")
			continue
		case strings.HasPrefix(a, "--log-file="):
			// A server-local temp path (os.CreateTemp), meaningless
			// forwarded to the client's own real mpv.
			continue
		case lastIsPath && i == n-1:
			mediaPath = a
			continue
		}
		passthrough = append(passthrough, a)
	}
	return ipcSocketPath, mediaPath, passthrough
}

type acceptResult struct {
	conn net.Conn
	err  error
}

// acceptBoth waits for both Seanime (over the IPC socket) and the agent
// (over TCP) to connect, in whichever order they happen to arrive in.
// The agent is expected to already be running and retrying its
// connection before this process even starts (see cmd/mpv-agent and the
// README) - the fixed TCP port means its very next retry succeeds within
// one retry interval of this listener coming up, which happens within
// milliseconds of process start, well before Seanime's own dial of the
// IPC socket even has a chance to occur.
func acceptBoth(ipcListener, tcpListener net.Listener, timeout time.Duration) (seanimeConn, agentConn net.Conn, err error) {
	ipcCh := make(chan acceptResult, 1)
	agentCh := make(chan acceptResult, 1)
	go func() { c, err := ipcListener.Accept(); ipcCh <- acceptResult{c, err} }()
	go func() { c, err := tcpListener.Accept(); agentCh <- acceptResult{c, err} }()

	deadline := time.After(timeout)
	for seanimeConn == nil || agentConn == nil {
		select {
		case a := <-ipcCh:
			if a.err != nil {
				return nil, nil, fmt.Errorf("accepting Seanime's IPC connection: %w", a.err)
			}
			seanimeConn = a.conn
			rlog.Printf("stub: Seanime connected to the IPC socket")
		case a := <-agentCh:
			if a.err != nil {
				return nil, nil, fmt.Errorf("accepting the agent's connection: %w", a.err)
			}
			agentConn = a.conn
			rlog.Printf("stub: agent connected over TCP")
		case <-deadline:
			switch {
			case seanimeConn == nil && agentConn == nil:
				return nil, nil, fmt.Errorf("timed out after %s: neither Seanime nor the agent connected", timeout)
			case seanimeConn == nil:
				return nil, nil, fmt.Errorf("timed out after %s: agent connected but Seanime never dialed the IPC socket", timeout)
			default:
				return nil, nil, fmt.Errorf("timed out after %s: Seanime connected but no agent dialed in - "+
					"is the agent running on the client machine, and can it reach this server's relay port?", timeout)
			}
		}
	}
	return seanimeConn, agentConn, nil
}

// waitForReady blocks (briefly) for the agent to confirm its local real
// mpv is up and its own relay loop has started. This is not required for
// correctness - IPC lines Seanime sends before the agent is ready are
// simply relayed a moment later, since TCP buffers them - but it gives
// the timing instrumentation a clean point to report against, and lets
// an early, unambiguous agent-side error (e.g. "mpv.exe not found")
// surface immediately instead of only showing up as a mysterious later
// timeout on Seanime's side.
func waitForReady(agentConn net.Conn, agentR *proto.Reader, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	_ = agentConn.SetReadDeadline(deadline)
	defer func() { _ = agentConn.SetReadDeadline(time.Time{}) }()

	for {
		c, err := agentR.ReadControl()
		if err != nil {
			rlog.Printf("stub: no ready confirmation from agent within %s (%v); relaying anyway", timeout, err)
			return
		}
		switch c.Kind {
		case proto.KindReady:
			rlog.Printf("stub: agent confirmed its real mpv is ready")
			return
		case proto.KindError:
			rlog.Printf("stub: agent reported an error while starting up: %s", c.Message)
			return
		default:
			// Unexpected control message this early; keep waiting for
			// ready/error up to the deadline.
		}
	}
}

func relaySeanimeToAgent(seanimeConn net.Conn, agentW *proto.Writer, rewriter *rewrite.IPCRewriter, errCh chan<- error) {
	lines := ipcline.NewReader(seanimeConn)
	for {
		line, err := lines.ReadLine()
		if err != nil {
			errCh <- fmt.Errorf("Seanime->agent: reading from Seanime: %w", err)
			return
		}
		rewritten := rewriter.RewriteOutgoing(line)
		if err := agentW.WriteIPCLine(rewritten); err != nil {
			errCh <- fmt.Errorf("Seanime->agent: writing to agent: %w", err)
			return
		}
	}
}

func relayAgentToSeanime(agentR *proto.Reader, seanimeConn net.Conn, rewriter *rewrite.IPCRewriter, errCh chan<- error) {
	for {
		f, err := agentR.ReadFrame()
		if err != nil {
			errCh <- fmt.Errorf("agent->Seanime: reading from agent: %w", err)
			return
		}
		switch f.Type {
		case proto.TypeIPCLine:
			rewritten := rewriter.RewriteIncoming(f.Payload)
			if _, err := seanimeConn.Write(append(rewritten, '\n')); err != nil {
				errCh <- fmt.Errorf("agent->Seanime: writing to Seanime: %w", err)
				return
			}
		case proto.TypeControl:
			var c proto.Control
			if json.Unmarshal(f.Payload, &c) == nil {
				rlog.Printf("stub: control from agent mid-session: kind=%s message=%q", c.Kind, c.Message)
				if c.Kind == proto.KindBye || c.Kind == proto.KindError {
					errCh <- fmt.Errorf("agent ended the session: %s", c.Message)
					return
				}
			}
		}
	}
}
