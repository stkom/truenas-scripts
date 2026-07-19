package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	configPath := flags.String("config", "fan-controller.json", "path to JSON configuration")
	dryRun := flags.Bool("dry-run", false, "read sensors but do not change fan settings")
	once := flags.Bool("once", false, "run one CPU and drive update, then exit")
	check := flags.Bool("check", false, "validate configuration, then exit")
	if err := flags.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration failed: %v\n", err)
		os.Exit(1)
	}
	if *check {
		fmt.Println("configuration valid")
		return
	}

	logger, closer, err := configureLogger(cfg.LogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging failed: %v\n", err)
		os.Exit(1)
	}
	if closer != nil {
		defer closer.Close()
	}
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runner := OSCommandRunner{}
	ipmi := NewIPMIClient(cfg.IPMI, runner, logger, *dryRun)
	smartctl := NewSmartctlClient(cfg.Smartctl, runner)
	controller := NewController(cfg, ipmi, smartctl, logger)
	if err := controller.Run(ctx, *once); err != nil {
		logger.Error("fan controller stopped", "event", "run_failed", "error", err)
		os.Exit(1)
	}
	logger.Info("fan controller stopped", "event", "shutdown_completed")
}
