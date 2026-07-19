package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCheck(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	data := `{"drives":{"devices":["/dev/sda"]},"log_path":""}`
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer

	code := run([]string{"-config", configPath, "-check"}, &stdout, &stderr)
	if code != 0 || strings.TrimSpace(stdout.String()) != "configuration valid" || stderr.Len() != 0 {
		t.Errorf("run() = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-unknown"}, &stdout, &stderr); code != 2 {
		t.Errorf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
