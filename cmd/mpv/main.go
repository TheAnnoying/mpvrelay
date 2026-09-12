// Command mpv is a drop-in replacement for the real mpv binary, run on
// the headless Seanime server. It creates the IPC socket/pipe Seanime
// dials, accepts a TCP connection from cmd/mpv-agent on the client
// machine, and relays JSON-IPC lines between them verbatim except for
// the rewriting in internal/rewrite. See ../../README.md for deployment.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mpvrelay/internal/proto"
	"mpvrelay/internal/rewrite"
	"mpvrelay/internal/rlog"
	"mpvrelay/internal/sockets"
)

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

	relayPort := getenv("SEANIME_MPV_RELAY_PORT", "43219")
	serverBaseURL := getenv("SEANIME_MPV_RELAY_SEANIME_URL", "")
	serverPassword := getenv("SEANIME_MPV_RELAY_PASSWORD", "")

	if serverBaseURL == "" {
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

	// One scanner for agentConn's whole life - see internal/proto's doc.
	agentScanner := bufio.NewScanner(agentConn)

	streamURL, err := rewrite.BuildStreamURL(serverBaseURL, serverPassword, mediaPath)
	if err != nil {
		return fmt.Errorf("building stream URL for %q: %w", mediaPath, err)
	}
	rlog.Printf("stub: built stream URL for real mpv to open")

	// mpv would otherwise title its window after the stream URL's own
	// query string (the base64 path + HMAC token), since it has no real
	// filesystem path to derive a title from.
	titleArg := "--force-media-title=" + filepath.Base(mediaPath)
	launch := proto.Launch{URL: streamURL, Args: append([]string{titleArg}, passthroughArgs...)}
	rlog.Printf("stub: sending launch to agent (%d passthrough args)", len(launch.Args))
	if err := writeJSONLine(agentConn, launch); err != nil {
		return fmt.Errorf("sending launch to agent: %w", err)
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

// writeJSONLine writes v as one line of JSON, newline-terminated.
func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// parseArgv splits Seanime's argv: the file path is always the last
// argument that isn't itself a flag.
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

// getenv returns the env var's value, or def if unset/empty, after
// stripping a stray cmd.exe quote pair: `set VAR="value"` leaves the
// quotes in os.Getenv, unlike a POSIX shell.
func getenv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	for len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' || first == '\'') && first == last {
			v = strings.TrimSpace(v[1 : len(v)-1])
			continue
		}
		break
	}
	if v == "" {
		return def
	}
	return v
}

type acceptResult struct {
	conn net.Conn
	err  error
}

// acceptBoth waits for Seanime (IPC socket) and the agent (TCP) to both
// connect, in whichever order they arrive in.
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

// waitForReady blocks briefly for the agent's one-line ack, mainly so an
// early agent-side error surfaces immediately instead of as a later
// timeout on Seanime's side.
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
