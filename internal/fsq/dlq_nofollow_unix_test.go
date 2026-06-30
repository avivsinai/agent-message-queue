//go:build darwin || linux

package fsq

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadDLQEnvelopeRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	_, _, err := ReadDLQEnvelope(path)
	if err == nil {
		t.Fatal("expected FIFO DLQ envelope to be rejected")
	}
}
