package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigAppliesDefaultsAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
		"drives": {
			"devices": ["/dev/sda", "/dev/sdb"],
			"poll_interval": "2m"
		},
		"cpu": {"reference_c": 50},
		"log_path": ""
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.CPU.ReferenceC != 50 {
		t.Errorf("CPU.ReferenceC = %v, want 50", cfg.CPU.ReferenceC)
	}
	if cfg.CPU.Fan.MinDuty != 30 {
		t.Errorf("CPU.Fan.MinDuty = %d, want default 30", cfg.CPU.Fan.MinDuty)
	}
	if cfg.Drives.PollInterval.Duration != 2*time.Minute {
		t.Errorf("Drives.PollInterval = %v, want 2m", cfg.Drives.PollInterval.Duration)
	}
	if cfg.LogMaxSizeMB != 5 || cfg.LogBackups != 2 {
		t.Errorf("log rotation = %d MB/%d backups, want 5/2", cfg.LogMaxSizeMB, cfg.LogBackups)
	}
	if cfg.CPU.MinimumTemperatureC != 5 || cfg.Drives.MinimumTemperatureC != 5 {
		t.Errorf("minimum temperatures = %v/%v, want 5/5", cfg.CPU.MinimumTemperatureC, cfg.Drives.MinimumTemperatureC)
	}
}

func TestConfigRejectsInvalidLogRotation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.LogMaxSizeMB = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "log_max_size_mb") {
		t.Fatalf("Validate() error = %v, want log size error", err)
	}

	cfg = DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.LogBackups = 11
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "log_backups") {
		t.Fatalf("Validate() error = %v, want backup count error", err)
	}
}

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"drives":{"devices":["/dev/sda"]},"mystery":true}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("LoadConfig() error = %v, want unknown field error", err)
	}
}

func TestConfigRejectsDuplicateZones(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.Drives.Fan.Zone = cfg.CPU.Fan.Zone

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "zones must differ") {
		t.Fatalf("Validate() error = %v, want duplicate zone error", err)
	}
}

func TestConfigRejectsInvalidMinimumTemperatures(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.CPU.MinimumTemperatureC = -21
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cpu.minimum_temperature_c") {
		t.Fatalf("Validate() error = %v, want CPU minimum-temperature error", err)
	}

	cfg = DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.Drives.MinimumTemperatureC = 150
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "drives.minimum_temperature_c") {
		t.Fatalf("Validate() error = %v, want drive minimum-temperature error", err)
	}
}
