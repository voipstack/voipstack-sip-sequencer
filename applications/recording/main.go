package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/voipstack/voipstack-sip-sequencer/applications/recording/recorder"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:5070", "SIP UDP listen address")
	dir := flag.String("dir", "recordings", "root directory for recorded files")
	mediaHost := flag.String("media-host", "127.0.0.1", "IP advertised in SDP answers")
	flag.Parse()

	cfg := recorder.Config{
		Listen:    *listen,
		Dir:       *dir,
		MediaHost: *mediaHost,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := recorder.Serve(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
