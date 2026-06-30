//go:build darwin || linux

package receipt

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	_, err := Read(path)
	if err == nil {
		t.Fatal("expected FIFO receipt to be rejected")
	}
}
