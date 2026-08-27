// Package rlog is a minimal timestamped logger.
//
// Everything is written to stderr so that stdout stays free for the "mpv"
// stub to emit the startup chatter Seanime's launcher scans for (see
// cmd/mpv). Timestamps use millisecond resolution because the timing budget
// this whole system lives inside of is measured in single-digit seconds
// (see internal/rewrite doc comment) — second-resolution timestamps would
// hide exactly the latency this logger exists to diagnose.
package rlog

import (
	"fmt"
	"os"
	"time"
)

var start = time.Now()

// Printf writes a timestamped line to stderr. The timestamp is both a
// wall-clock time (for correlating stub and agent logs, which run on
// different machines with their own clocks) and a "+Nms since process
// start" offset (for reasoning about the latency budget on a single
// machine without doing clock arithmetic by hand).
func Printf(format string, args ...any) {
	now := time.Now()
	prefix := fmt.Sprintf("%s (+%6dms) ", now.Format("15:04:05.000"), now.Sub(start).Milliseconds())
	fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
}
