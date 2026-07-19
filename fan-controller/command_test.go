package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfiguredCommandPrependsArgs(t *testing.T) {
	var gotPath string
	var gotArgs []string
	runner := runnerFunc(func(_ context.Context, path string, args ...string) (CommandResult, error) {
		gotPath = path
		gotArgs = append([]string{}, args...)
		return CommandResult{}, nil
	})
	command := NewConfiguredCommand(CommandConfig{
		Path: "tool", Args: []string{"--global"}, Timeout: Duration{time.Second},
	}, runner)

	if _, err := command.Run(context.Background(), "action"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "tool" || !reflect.DeepEqual(gotArgs, []string{"--global", "action"}) {
		t.Errorf("command = %q %v", gotPath, gotArgs)
	}
}

func TestConfiguredCommandNormalizesTimeout(t *testing.T) {
	runner := runnerFunc(func(ctx context.Context, _ string, _ ...string) (CommandResult, error) {
		<-ctx.Done()
		return CommandResult{}, errors.New("process killed")
	})
	command := NewConfiguredCommand(CommandConfig{Path: "tool", Timeout: Duration{time.Millisecond}}, runner)

	_, err := command.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out after 1ms") {
		t.Fatalf("Run() error = %v, want normalized timeout", err)
	}
}
