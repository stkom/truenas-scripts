package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type CommandRunner interface {
	Run(ctx context.Context, path string, args ...string) (CommandResult, error)
}

type OSCommandRunner struct{}

func (OSCommandRunner) Run(ctx context.Context, path string, args ...string) (CommandResult, error) {
	command := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

type ConfiguredCommand struct {
	cfg    CommandConfig
	runner CommandRunner
}

func NewConfiguredCommand(cfg CommandConfig, runner CommandRunner) ConfiguredCommand {
	return ConfiguredCommand{cfg: cfg, runner: runner}
}

func (command ConfiguredCommand) Run(ctx context.Context, args ...string) (CommandResult, error) {
	commandCtx, cancel := context.WithTimeout(ctx, command.cfg.Timeout.Duration)
	defer cancel()

	allArgs := append(append([]string{}, command.cfg.Args...), args...)
	result, err := command.runner.Run(commandCtx, command.cfg.Path, allArgs...)
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("%s timed out after %s", command.cfg.Path, command.cfg.Timeout.Duration)
	}
	if commandCtx.Err() != nil {
		return result, fmt.Errorf("%s canceled: %w", command.cfg.Path, commandCtx.Err())
	}
	if err != nil {
		return result, fmt.Errorf("error running %s: %w", command.cfg.Path, err)
	}
	return result, nil
}
