package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

type rotatingFile struct {
	path     string
	maxBytes int64
	backups  int
	file     *os.File
	size     int64
	mu       sync.Mutex
}

func openRotatingFile(path string, maxBytes int64, backups int) (*rotatingFile, error) {
	writer := &rotatingFile{path: path, maxBytes: maxBytes, backups: backups}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (writer *rotatingFile) open() error {
	file, err := os.OpenFile(writer.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	writer.file = file
	writer.size = info.Size()
	return nil
}

func (writer *rotatingFile) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.file == nil {
		if err := writer.open(); err != nil {
			return 0, err
		}
	}
	if writer.size > 0 && writer.size+int64(len(data)) > writer.maxBytes {
		if err := writer.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := writer.file.Write(data)
	writer.size += int64(written)
	return written, err
}

func (writer *rotatingFile) Close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.file == nil {
		return nil
	}
	err := writer.file.Close()
	writer.file = nil
	return err
}

func (writer *rotatingFile) rotate() error {
	err := writer.file.Close()
	writer.file = nil
	if err != nil {
		return err
	}
	if writer.backups == 0 {
		if err := os.Remove(writer.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		for index := writer.backups; index >= 1; index-- {
			destination := fmt.Sprintf("%s.%d", writer.path, index)
			if index == writer.backups {
				if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			source := writer.path
			if index > 1 {
				source = fmt.Sprintf("%s.%d", writer.path, index-1)
			}
			if err := os.Rename(source, destination); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return writer.open()
}

func configureLogger(stdout io.Writer, logPath string, maxSizeMB, backups int) (*slog.Logger, io.Closer, error) {
	writers := []io.Writer{stdout}
	var closer io.Closer
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
			return nil, nil, fmt.Errorf("error creating log directory: %w", err)
		}
		file, err := openRotatingFile(logPath, int64(maxSizeMB)*1024*1024, backups)
		if err != nil {
			return nil, nil, fmt.Errorf("error opening log: %w", err)
		}
		writers = append(writers, file)
		closer = file
	}
	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler), closer, nil
}
