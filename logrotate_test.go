package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotatingLogWriterRotatesBySizeAndKeepsBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.log")
	writer, err := newRotatingLogWriter(path, 1, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	writer.maxSizeBytes = 10

	for i := 0; i < 4; i++ {
		if _, err := writer.Write([]byte("123456789\n")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "gateway-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("rotated logs = %d, want 2: %v", len(matches), matches)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active log missing: %v", err)
	}
}

func TestRotatingLogWriterRotatesByInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.log")
	writer, err := newRotatingLogWriter(path, 0, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writer.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	writer.nextRotation = time.Now().Add(-time.Second)
	if _, err := writer.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "gateway-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("rotated logs = %d, want 1: %v", len(matches), matches)
	}
}
