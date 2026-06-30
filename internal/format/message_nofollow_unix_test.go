//go:build darwin || linux

package format

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadMessageFileRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	_, err := ReadMessageFile(path)
	if err == nil {
		t.Fatal("expected FIFO message file to be rejected")
	}
}

func TestReadHeaderFileRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	_, err := ReadHeaderFile(path)
	if err == nil {
		t.Fatal("expected FIFO header file to be rejected")
	}
}
