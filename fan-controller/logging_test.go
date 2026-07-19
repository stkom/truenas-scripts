package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingFileBoundsBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.log")
	writer, err := openRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first\n", "second\n", "third\n"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	assertFileContents(t, path, "third\n")
	assertFileContents(t, path+".1", "second\n")
	assertFileContents(t, path+".2", "first\n")
}

func TestConfigureLoggerWithoutFile(t *testing.T) {
	var output bytes.Buffer
	logger, closer, err := configureLogger(&output, "", 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		t.Fatal("closer is non-nil without log path")
	}
	logger.Info("hello")
	if !bytes.Contains(output.Bytes(), []byte("hello")) {
		t.Errorf("log output = %q", output.String())
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("%s = %q, want %q", path, data, want)
	}
}
