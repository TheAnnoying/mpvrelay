// Package rlog is a minimal timestamped logger, writing to stderr only
// so stdout stays free for cmd/mpv's Seanime-scanned startup line.
package rlog

import (
	"fmt"
	"os"
	"time"
)

var start = time.Now()

// Printf logs a wall-clock time (to correlate stub/agent logs across
// machines) plus a "+Nms since start" offset (to reason about latency).
func Printf(format string, args ...any) {
	now := time.Now()
	prefix := fmt.Sprintf("%s (+%6dms) ", now.Format("15:04:05.000"), now.Sub(start).Milliseconds())
	fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
}
