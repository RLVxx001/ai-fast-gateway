package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rotatingLogWriter struct {
	mu             sync.Mutex
	path           string
	maxSizeBytes   int64
	maxBackups     int
	rotateInterval time.Duration
	nextRotation   time.Time
	file           *os.File
	size           int64
}

func newRotatingLogWriter(path string, maxSizeMB int, maxBackups int, rotateInterval time.Duration) (*rotatingLogWriter, error) {
	writer := &rotatingLogWriter{
		path:           path,
		maxBackups:     maxBackups,
		rotateInterval: rotateInterval,
	}
	if maxSizeMB > 0 {
		writer.maxSizeBytes = int64(maxSizeMB) * 1024 * 1024
	}
	if rotateInterval > 0 {
		writer.nextRotation = time.Now().Add(rotateInterval)
	}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.rotateIfNeeded(len(p), time.Now()); err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingLogWriter) open() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingLogWriter) rotateIfNeeded(incoming int, now time.Time) error {
	if w.file == nil {
		return w.open()
	}
	bySize := w.maxSizeBytes > 0 && w.size > 0 && w.size+int64(incoming) > w.maxSizeBytes
	byInterval := w.rotateInterval > 0 && !w.nextRotation.IsZero() && !now.Before(w.nextRotation)
	if !bySize && !byInterval {
		return nil
	}
	if err := w.rotate(now); err != nil {
		return err
	}
	if w.rotateInterval > 0 {
		for !now.Before(w.nextRotation) {
			w.nextRotation = w.nextRotation.Add(w.rotateInterval)
		}
	}
	return nil
}

func (w *rotatingLogWriter) rotate(now time.Time) error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	if w.size > 0 {
		rotated := w.rotatedPath(now)
		if err := os.Rename(w.path, rotated); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := w.cleanup(); err != nil {
		return err
	}
	return w.open()
}

func (w *rotatingLogWriter) rotatedPath(now time.Time) string {
	ext := filepath.Ext(w.path)
	stem := strings.TrimSuffix(w.path, ext)
	base := stem + "-" + now.Format("20060102-150405") + ext
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	for i := 1; ; i++ {
		candidate := stem + "-" + now.Format("20060102-150405") + "-" + strconv.Itoa(i) + ext
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func (w *rotatingLogWriter) cleanup() error {
	if w.maxBackups <= 0 {
		return nil
	}
	ext := filepath.Ext(w.path)
	stem := strings.TrimSuffix(w.path, ext)
	matches, err := filepath.Glob(stem + "-*" + ext)
	if err != nil {
		return err
	}
	if len(matches) <= w.maxBackups {
		return nil
	}
	sort.Strings(matches)
	for _, path := range matches[:len(matches)-w.maxBackups] {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
