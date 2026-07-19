package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fan-controller", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "fan-controller.json", "path to JSON configuration")
	dryRun := flags.Bool("dry-run", false, "read sensors but do not change fan settings")
	once := flags.Bool("once", false, "run one CPU and drive update, then exit")
	check := flags.Bool("check", false, "validate configuration, then exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "configuration failed: %v\n", err)
		return 1
	}
	if *check {
		fmt.Fprintln(stdout, "configuration valid")
		return 0
	}

	logger, closer, err := configureLogger(stdout, cfg.LogPath, cfg.LogMaxSizeMB, cfg.LogBackups)
	if err != nil {
		fmt.Fprintf(stderr, "logging failed: %v\n", err)
		return 1
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
	controller := NewController(cfg, ipmi, NewCPUReader(cfg.CPU, ipmi), NewDriveReader(smartctl), logger)
	if err := controller.Run(ctx, *once); err != nil {
		logger.Error("fan controller stopped", "event", "run_failed", "error", err)
		return 1
	}
	logger.Info("fan controller stopped", "event", "shutdown_completed")
	return 0
}
