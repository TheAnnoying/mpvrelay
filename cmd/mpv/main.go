// Command mpv is a drop-in replacement for the real mpv binary, run on
// the headless Seanime server. Configuration is via env vars; see
// ../../README.md for deployment.
package main

import (
	"os"
	"strings"

	"github.com/TheAnnoying/mpvrelay/internal/rlog"
	"github.com/TheAnnoying/mpvrelay/stub"
)

func main() {
	cfg := stub.Config{
		RelayPort: getenv("SEANIME_MPV_RELAY_PORT", "43219"),
		ServerURL: getenv("SEANIME_MPV_RELAY_SEANIME_URL", ""),
		Password:  getenv("SEANIME_MPV_RELAY_PASSWORD", ""),
	}
	if err := stub.Run(os.Args[1:], cfg); err != nil {
		rlog.Printf("stub: fatal: %v", err)
		os.Exit(1)
	}
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
