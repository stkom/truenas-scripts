package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

func configureLogger(logPath string) (*slog.Logger, io.Closer, error) {
	writers := []io.Writer{os.Stdout}
	var closer io.Closer
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
			return nil, nil, fmt.Errorf("error creating log directory: %w", err)
		}
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, nil, fmt.Errorf("error opening log: %w", err)
		}
		writers = append(writers, file)
		closer = file
	}
	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler), closer, nil
}
