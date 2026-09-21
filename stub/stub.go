// Package stub is the server-side half of mpvrelay: a drop-in replacement
// for the real mpv binary, run on the headless Seanime server. It creates
// the IPC socket/pipe Seanime dials, accepts a TCP connection from the
// agent (package agent) on the client machine, and relays JSON-IPC lines
// between them verbatim except for the rewriting in internal/rewrite.
// cmd/mpv is a thin wrapper around Run; a Go app can also embed it (e.g.
// by calling Run when it is re-executed under the name "mpv"). See
// ../README.md for deployment.
package stub

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/TheAnnoying/mpvrelay/internal/proto"
	"github.com/TheAnnoying/mpvrelay/internal/rewrite"
	"github.com/TheAnnoying/mpvrelay/internal/rlog"
	"github.com/TheAnnoying/mpvrelay/internal/sockets"
)

const (
	acceptTimeout = 30 * time.Second
	readyTimeout  = 15 * time.Second
)

// Config is what the stub needs besides mpv's own argv.
type Config struct {
	RelayPort string // TCP port the agent dials; default "43219"
	ServerURL string // Seanime's base URL as reachable from the agent (required)
	Password  string // Seanime's plaintext server password, if one is set
}

// Run is the whole stub process: args is mpv's argv without the program
// name (os.Args[1:]). It blocks for one playback session and returns
// when it ends.
func Run(args []string, cfg Config) error {
	ipcSocketPath, mediaPath, passthroughArgs := parseArgv(args)

	// Seanime's launcher waits for a line of stdout before proceeding.
	fmt.Println("mpvrelay-stub: starting")

	if ipcSocketPath == "" {
		return errors.New("no --input-ipc-server=<path> argument in argv; " +
			"this binary is only meant to be launched by Seanime, not run by hand")
	}
	if mediaPath == "" {
		return errors.New("no media file path in argv; " +
			"this binary is only meant to be launched by Seanime, not run by hand")
	}

	relayPort := cfg.RelayPort
	if relayPort == "" {
		relayPort = "43219"
	}
	serverBaseURL, serverPassword := cfg.ServerURL, cfg.Password

	if serverBaseURL == "" {
		return fmt.Errorf("Config.ServerURL (SEANIME_MPV_RELAY_SEANIME_URL) is not set; cannot build a stream URL for %q", mediaPath)
	}

	ipcListener, err := sockets.Listen(ipcSocketPath)
	if err != nil {
		return fmt.Errorf("listening on IPC socket: %w", err)
	}
	defer ipcListener.Close()

	tcpListener, err := net.Listen("tcp", ":"+relayPort)
	if err != nil {
		return fmt.Errorf("listening on relay TCP port %s: %w", relayPort, err)
	}
	defer tcpListener.Close()
	rlog.Printf("stub: waiting for agent on port %s (streaming from %s)", relayPort, serverBaseURL)

	seanimeConn, agentConn, err := acceptBoth(ipcListener, tcpListener, acceptTimeout)
	if err != nil {
		return err
	}
	defer seanimeConn.Close()
	defer agentConn.Close()

	agentScanner := bufio.NewScanner(agentConn) // reused for the conn's whole life, see internal/proto

	var streamURL, title string
	if rewrite.IsMediaURL(mediaPath) {
		// Torrent streaming: Seanime already gives us a URL, to its own
		// embedded torrent-stream server - bound to a loopback address
		// that only means anything on the server's own machine.
		streamURL, title, err = rewrite.RewriteTorrentStreamURL(serverBaseURL, mediaPath)
		if err != nil {
			return fmt.Errorf("rewriting torrent-stream URL %q: %w", mediaPath, err)
		}
	} else {
		streamURL, err = rewrite.BuildStreamURL(serverBaseURL, serverPassword, mediaPath)
		if err != nil {
			return fmt.Errorf("building stream URL for %q: %w", mediaPath, err)
		}
		title = filepath.Base(mediaPath)
	}

	// mpv would otherwise title its window after the URL's own query
	// string (the base64 path + HMAC token), since it has no real
	// filesystem path to derive a title from.
	titleArg := "--force-media-title=" + title
	launch := proto.Launch{URL: streamURL, Args: append([]string{titleArg}, passthroughArgs...)}
	if err := writeJSONLine(agentConn, launch); err != nil {
	}

	waitForReady(agentConn, agentScanner, readyTimeout)

	rewriter := rewrite.NewIPCRewriter(serverBaseURL, serverPassword)

	errCh := make(chan error, 2)
	go relaySeanimeToAgent(seanimeConn, agentConn, rewriter, errCh)
	go relayAgentToSeanime(agentScanner, seanimeConn, rewriter, errCh)

	sessionErr := <-errCh
	rlog.Printf("stub: session ended: %v", sessionErr)

	return nil
}

func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// Seanime always appends the file path last, after every flag.
func parseArgv(args []string) (ipcSocketPath, mediaPath string, passthrough []string) {
	n := len(args)
	lastIsPath := n > 0 && !strings.HasPrefix(args[n-1], "-")

	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--input-ipc-server="):
			ipcSocketPath = strings.TrimPrefix(a, "--input-ipc-server=")
			continue
		case strings.HasPrefix(a, "--log-file="):
			continue // server-local temp path, meaningless on the client
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

// Not required for correctness (IPC lines sent before this returns are
// just relayed once the agent connects) - lets an agent-side startup
// error surface immediately instead of as a later timeout on Seanime's
// side.
func waitForReady(agentConn net.Conn, agentScanner *bufio.Scanner, timeout time.Duration) {
	_ = agentConn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = agentConn.SetReadDeadline(time.Time{}) }()

	if !agentScanner.Scan() {
		rlog.Printf("stub: no ready confirmation from agent within %s (%v); relaying anyway", timeout, agentScanner.Err())
		return
	}
	var ack proto.Ack
	if err := json.Unmarshal(agentScanner.Bytes(), &ack); err != nil {
		rlog.Printf("stub: malformed ready confirmation from agent: %v", err)
		return
	}
	if ack.Error != "" {
		rlog.Printf("stub: agent reported an error while starting up: %s", ack.Error)
		return
	}
	rlog.Printf("stub: agent confirmed its real mpv is ready")
}

func relaySeanimeToAgent(seanimeConn, agentConn net.Conn, rewriter *rewrite.IPCRewriter, errCh chan<- error) {
	scanner := bufio.NewScanner(seanimeConn)
	for scanner.Scan() {
		rewritten := rewriter.RewriteOutgoing(scanner.Bytes())
		if _, err := agentConn.Write(append(rewritten, '\n')); err != nil {
			errCh <- fmt.Errorf("Seanime->agent: writing to agent: %w", err)
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	errCh <- fmt.Errorf("Seanime->agent: reading from Seanime: %w", err)
}

func relayAgentToSeanime(agentScanner *bufio.Scanner, seanimeConn net.Conn, rewriter *rewrite.IPCRewriter, errCh chan<- error) {
	for agentScanner.Scan() {
		rewritten := rewriter.RewriteIncoming(agentScanner.Bytes())
		if _, err := seanimeConn.Write(append(rewritten, '\n')); err != nil {
			errCh <- fmt.Errorf("agent->Seanime: writing to Seanime: %w", err)
			return
		}
	}
	err := agentScanner.Err()
	if err == nil {
		err = io.EOF
	}
	errCh <- fmt.Errorf("agent->Seanime: reading from agent: %w", err)
}
