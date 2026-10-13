package app

import (
	"bytes"
	"io"
	"sync"
)

// secretLogWriter retains only a possible credential prefix between writes, so
// child output remains live without leaking a password split over pipe reads.
// Each child stream gets its own writer to avoid mixing stdout/stderr prefixes.
type secretLogWriter struct {
	mu      sync.Mutex
	dest    io.Writer
	secret  []byte
	pending []byte
}

func (w *secretLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.secret) == 0 {
		return w.dest.Write(p)
	}
	w.pending = append(w.pending, p...)
	w.pending = bytes.ReplaceAll(w.pending, w.secret, []byte("[REDACTED]"))
	keep := len(w.secret) - 1
	if keep > len(w.pending) {
		keep = len(w.pending)
	}
	for keep > 0 && !bytes.Equal(w.pending[len(w.pending)-keep:], w.secret[:keep]) {
		keep--
	}
	safe := len(w.pending) - keep
	if safe > 0 {
		n, err := w.dest.Write(w.pending[:safe])
		if err != nil {
			return len(p), err
		}
		if n != safe {
			return len(p), io.ErrShortWrite
		}
	}
	w.pending = append(w.pending[:0], w.pending[safe:]...)
	return len(p), nil
}

// A truncated credential prefix at process exit is also withheld from logs.
func (w *secretLogWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	w.pending = nil
	_, err := io.WriteString(w.dest, "[REDACTED]")
	return err
}
