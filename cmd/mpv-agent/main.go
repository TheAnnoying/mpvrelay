// Command mpv-agent runs persistently on the client machine that has a
// real screen and real mpv. See ../../README.md for flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/TheAnnoying/mpvrelay/agent"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	agent.Run(ctx, *server, *mpvPath)
}
