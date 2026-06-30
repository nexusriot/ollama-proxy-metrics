package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingWriter_RotatesAtMaxBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	w, err := NewRotatingWriter(path, 10, 2) // rotate after 10 bytes
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	// Two 8-byte writes: the second crosses the 10-byte threshold and rotates.
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected rotated backup %s.1: %v", path, err)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != "abcdefgh" {
		t.Errorf("current file = %q, want second write", string(cur))
	}
}

func TestRotatingWriter_NoRotationWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := NewRotatingWriter(path, 0, 3) // 0 disables rotation
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()
	for i := 0; i < 100; i++ {
		_, _ = w.Write([]byte("some log line\n"))
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Error("expected no rotation when maxBytes=0")
	}
}
