package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSmartctlReadTemperature(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ ...string) (CommandResult, error) {
		return CommandResult{Stdout: []byte(`{
			"smartctl":{"exit_status":8},
			"temperature":{"current":37}
		}`), ExitCode: 8}, nil
	})
	client := NewSmartctlClient(CommandConfig{Path: "smartctl", Timeout: Duration{time.Second}}, runner)

	reading := client.ReadTemperature(context.Background(), "/dev/sda")
	if reading.State != DriveActive || reading.Temperature != 37 {
		t.Errorf("reading = %+v, want active at 37 C", reading)
	}
	if reading.Warning == "" {
		t.Error("reading.Warning is empty, want SMART health warning")
	}
}

func TestSmartctlRecognizesStandby(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ ...string) (CommandResult, error) {
		return CommandResult{Stdout: []byte(`{
			"smartctl":{"exit_status":2},
			"power_mode":"STANDBY"
		}`), ExitCode: 2}, nil
	})
	client := NewSmartctlClient(CommandConfig{Path: "smartctl", Timeout: Duration{time.Second}}, runner)

	reading := client.ReadTemperature(context.Background(), "/dev/sda")
	if reading.State != DriveStandby {
		t.Errorf("reading = %+v, want standby", reading)
	}
}

func TestSmartctlDoesNotMistakeOpenFailureForStandby(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, _ string, _ ...string) (CommandResult, error) {
		return CommandResult{Stdout: []byte(`{"smartctl":{"exit_status":2}}`), ExitCode: 2}, nil
	})
	client := NewSmartctlClient(CommandConfig{Path: "smartctl", Timeout: Duration{time.Second}}, runner)

	reading := client.ReadTemperature(context.Background(), "/dev/missing")
	if reading.State != DriveFailed {
		t.Errorf("reading = %+v, want failed", reading)
	}
}

func TestReadHwmonCPUTemperatureUsesHottestPackage(t *testing.T) {
	root := t.TempDir()
	writeHwmon(t, root, "hwmon0", "coretemp", map[string]string{
		"temp1_label": "Package id 0\n",
		"temp1_input": "42500\n",
		"temp2_label": "Core 0\n",
		"temp2_input": "99000\n",
	})
	writeHwmon(t, root, "hwmon1", "k10temp", map[string]string{
		"temp1_label": "Tctl\n",
		"temp1_input": "51750\n",
	})

	got, err := ReadHwmonCPUTemperature(root)
	if err != nil {
		t.Fatalf("ReadHwmonCPUTemperature() error = %v", err)
	}
	if got != 51.75 {
		t.Errorf("ReadHwmonCPUTemperature() = %v, want 51.75", got)
	}
}

func writeHwmon(t *testing.T, root, directory, name string, files map[string]string) {
	t.Helper()
	path := filepath.Join(root, directory)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "name"), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	for filename, contents := range files {
		if err := os.WriteFile(filepath.Join(path, filename), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
