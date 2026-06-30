// Package logging provides a minimal size-based rotating file writer, so the
// structured request log does not grow unbounded. It deliberately avoids an
// external dependency (e.g. lumberjack): the proxy emits one small JSON line per
// request, so naive size-triggered rotation is plenty.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter is an io.WriteCloser that rotates the target file once it
// exceeds MaxBytes, keeping up to MaxBackups historical files
// (name.1, name.2, …). It is safe for concurrent use.
type RotatingWriter struct {
	path       string
	maxBytes   int64
	maxBackups int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// NewRotatingWriter opens path for appending and rotates it when it grows past
// maxBytes, retaining maxBackups rotated files. A maxBytes <= 0 disables
// rotation (plain append).
func NewRotatingWriter(path string, maxBytes int64, maxBackups int) (*RotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &RotatingWriter{
		path:       path,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
		f:          f,
		size:       info.Size(),
	}, nil
}

// Write implements io.Writer, rotating first if the file would exceed MaxBytes.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.maxBytes > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the underlying file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// rotate closes the current file, shifts backups, and opens a fresh file.
// Caller must hold w.mu.
func (w *RotatingWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	// Drop the oldest, then shift name.(i) -> name.(i+1).
	if w.maxBackups > 0 {
		_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.maxBackups))
		for i := w.maxBackups - 1; i >= 1; i-- {
			_ = os.Rename(
				fmt.Sprintf("%s.%d", w.path, i),
				fmt.Sprintf("%s.%d", w.path, i+1),
			)
		}
		_ = os.Rename(w.path, w.path+".1")
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	w.size = 0
	return nil
}
