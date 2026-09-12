// Command mpv-agent runs persistently on the client machine that has a
// real screen and real mpv. It dials the server-side stub (cmd/mpv) over
// TCP, and on each session launches real mpv locally and tunnels its
// IPC socket/pipe back over that connection - byte for byte, with zero
// interpretation of the traffic (see internal/rewrite for why that lives
// only on the server side). See ../../README.md for flags.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"mpvrelay/internal/proto"
	"mpvrelay/internal/rlog"
	"mpvrelay/internal/sockets"
)

const (
	retryInterval    = 500 * time.Millisecond
	dialTimeout      = 3 * time.Second
	pipeReadyTimeout = 10 * time.Second
	handshakeTimeout = 10 * time.Second
)

func main() {
	server := flag.String("server", "", "host:port of the mpv stub's relay port, e.g. myserver.local:43219 (required)")
	mpvPath := flag.String("mpv", "mpv", "path to the real mpv/mpv.exe binary")
	flag.Parse()

	if *server == "" {
		fmt.Fprintln(os.Stderr, "agent: fatal: -server is required (expected host:port, e.g. myserver.local:43219)")
		flag.Usage()
		os.Exit(1)
	}

	rlog.Printf("agent: starting; server=%s mpv=%s", *server, *mpvPath)

	for {
		conn, err := net.DialTimeout("tcp", *server, dialTimeout)
		if err != nil {
			time.Sleep(retryInterval)
			continue
		}
		rlog.Printf("agent: connected to stub at %s", *server)
		runSession(conn, *mpvPath)
		rlog.Printf("agent: session ended, resuming retry loop")
	}
}

// runSession handles one TCP connection to the stub: handshake, then
// relaying until either side disconnects or mpv exits. It never returns
// an error - any failure just means "go back to redialing" in main.
func runSession(conn net.Conn, mpvPath string) {
	defer conn.Close()

	// One scanner for this conn's entire life - see internal/proto's doc.
	scanner := bufio.NewScanner(conn)

	// Bounded deadline for the handshake: a plain TCP connect can succeed
	// even when the connection never delivers data (stale port-forward,
	// a NAT that blackholes the rest), which would otherwise wedge this
	// agent forever instead of returning to the retry loop.
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))

	if !scanner.Scan() {
		rlog.Printf("agent: did not receive launch from stub within %s: %v", handshakeTimeout, scanner.Err())
		return
	}
	var launch proto.Launch
	if err := json.Unmarshal(scanner.Bytes(), &launch); err != nil {
		rlog.Printf("agent: malformed launch message from stub: %v", err)
		return
	}
	rlog.Printf("agent: received launch (url=%s, args=%v)", launch.URL, launch.Args)

	_ = conn.SetReadDeadline(time.Time{})

	pipeName := freshPipeName()
	cmd, err := startMpv(mpvPath, pipeName, launch)
	if err != nil {
		rlog.Printf("agent: ERROR: mpv could not be started (is -mpv=%q correct?): %v", mpvPath, err)
		_ = writeAck(conn, fmt.Sprintf("starting mpv: %v", err))
		return
	}
	rlog.Printf("agent: started mpv.exe (pid=%d), waiting for its IPC pipe", cmd.Process.Pid)

	mpvExited := make(chan error, 1)
	go func() { mpvExited <- cmd.Wait() }()

	pipeConn, err := dialPipeWithRetry(pipeName, pipeReadyTimeout, mpvExited)
	if err != nil {
		rlog.Printf("agent: ERROR: mpv started but never opened its IPC pipe: %v", err)
		_ = writeAck(conn, fmt.Sprintf("connecting to mpv IPC pipe: %v", err))
		killMpv(cmd, mpvExited)
		return
	}
	defer pipeConn.Close()
	rlog.Printf("agent: connected to mpv's IPC pipe, relaying")

	if err := writeAck(conn, ""); err != nil {
		rlog.Printf("agent: failed to send ready: %v", err)
		killMpv(cmd, mpvExited)
		return
	}

	errCh := make(chan error, 3)
	go relayTCPToPipe(scanner, pipeConn, errCh)
	go relayPipeToTCP(pipeConn, conn, errCh)
	go func() {
		err := <-mpvExited
		errCh <- fmt.Errorf("mpv.exe exited: %v", err)
	}()

	sessionErr := <-errCh
	rlog.Printf("agent: relay ended: %v", sessionErr)

	killMpv(cmd, mpvExited)
}

// writeAck sends the one-line ack the stub waits for: empty errMsg means
// mpv is up and relaying is starting; non-empty means startup failed.
func writeAck(conn net.Conn, errMsg string) error {
	data, err := json.Marshal(proto.Ack{Error: errMsg})
	if err != nil {
		return err
	}
	_, err = conn.Write(append(data, '\n'))
	return err
}

func startMpv(mpvPath, pipeName string, launch proto.Launch) (*exec.Cmd, error) {
	args := append([]string{}, launch.Args...)
	args = append(args, "--input-ipc-server="+pipeName, launch.URL)

	cmd := exec.Command(mpvPath, args...)
	cmd.Stdout = nil // drain instead of inheriting, so mpv never blocks on a full pipe buffer
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// freshPipeName returns a unique local IPC endpoint path for
// --input-ipc-server: a named pipe on Windows, a Unix domain socket
// elsewhere.
func freshPipeName() string {
	id := fmt.Sprintf("mpvrelay_%d_%d", os.Getpid(), time.Now().UnixNano())
	if runtime.GOOS == "windows" {
		return `\\.\pipe\` + id
	}
	return filepath.Join(os.TempDir(), id+".sock")
}

// dialPipeWithRetry retries because mpv needs a moment after Start()
// before its IPC pipe is listening; it gives up early if mpv already
// exited instead of waiting out the full timeout.
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

func relayTCPToPipe(scanner *bufio.Scanner, pipeConn net.Conn, errCh chan<- error) {
	for scanner.Scan() {
		if _, err := pipeConn.Write(append(scanner.Bytes(), '\n')); err != nil {
			errCh <- fmt.Errorf("stub->mpv: writing to mpv pipe: %w", err)
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	errCh <- fmt.Errorf("stub->mpv: reading from stub: %w", err)
}

func relayPipeToTCP(pipeConn, stubConn net.Conn, errCh chan<- error) {
	scanner := bufio.NewScanner(pipeConn)
	for scanner.Scan() {
		if _, err := stubConn.Write(append(scanner.Bytes(), '\n')); err != nil {
			errCh <- fmt.Errorf("mpv->stub: writing to stub: %w", err)
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	errCh <- fmt.Errorf("mpv->stub: reading from mpv pipe: %w", err)
}

// killMpv ensures mpv is gone before session cleanup finishes, so the
// next session gets a clean start.
func killMpv(cmd *exec.Cmd, mpvExited <-chan error) {
	select {
	case <-mpvExited:
		return // already exited on its own
	default:
	}
	_ = cmd.Process.Kill()
	select {
	case <-mpvExited:
	case <-time.After(3 * time.Second):
		rlog.Printf("agent: mpv.exe did not exit within 3s of being killed")
	}
}
