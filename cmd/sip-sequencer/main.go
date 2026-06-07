package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/voipstack/voipstack-sip-sequencer/internal/b2bua"
	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to the YAML configuration file")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var lvl slog.LevelVar
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "error: log level: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &lvl})))

	slog.Info("starting voipstack-sip-sequencer", "version", version)

	var opts []b2bua.Option
	if cfg.Observability.Listen != "" {
		opts = append(opts, b2bua.WithMetrics(b2bua.NewPromMetrics()))
	}

	eng, err := b2bua.New(cfg, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: start engine: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := eng.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: engine: %v\n", err)
	}

	if err := eng.Shutdown(); err != nil {
		fmt.Fprintf(os.Stderr, "error: shutdown: %v\n", err)
	}
}
