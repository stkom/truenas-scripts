package main

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestIPMISetZoneDuty(t *testing.T) {
	var gotPath string
	var gotArgs []string
	runner := runnerFunc(func(_ context.Context, path string, args ...string) (CommandResult, error) {
		gotPath = path
		gotArgs = append([]string{}, args...)
		return CommandResult{}, nil
	})
	cfg := CommandConfig{Path: "ipmitool", Args: []string{"-I", "open"}, Timeout: Duration{time.Second}}
	client := NewIPMIClient(cfg, runner, slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	if err := client.SetZoneDuty(context.Background(), 1, 50); err != nil {
		t.Fatalf("SetZoneDuty() error = %v", err)
	}
	wantArgs := []string{"-I", "open", "raw", "0x30", "0x70", "0x66", "0x01", "0x01", "0x32"}
	if gotPath != "ipmitool" || !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("command = %q %v, want ipmitool %v", gotPath, gotArgs, wantArgs)
	}
}

func TestIPMIReadTemperature(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ ...string) (CommandResult, error) {
		return CommandResult{Stdout: []byte("Sensor Reading        : 38 (+/- 0) degrees C\n")}, nil
	})
	cfg := CommandConfig{Path: "ipmitool", Timeout: Duration{time.Second}}
	client := NewIPMIClient(cfg, runner, slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	got, err := client.ReadTemperature(context.Background(), "CPU Temp")
	if err != nil {
		t.Fatalf("ReadTemperature() error = %v", err)
	}
	if got != 38 {
		t.Errorf("ReadTemperature() = %v, want 38", got)
	}
}

func TestParseHexByte(t *testing.T) {
	got, err := parseHexByte([]byte(" 32\n"))
	if err != nil || got != 50 {
		t.Fatalf("parseHexByte() = %d, %v; want 50, nil", got, err)
	}
}
