package main

import (
	"bytes"
	"context"
	"errors"
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
