//go:build darwin || linux

package selfupgrade

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadEmbeddedVersionDoesNotBlockOnFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ReadEmbeddedVersion(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ReadEmbeddedVersion() accepted a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadEmbeddedVersion() blocked on a FIFO")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
