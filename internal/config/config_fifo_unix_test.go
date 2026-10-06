//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Pro review of za4 r3: the existence check read config.json under the config
// lock, so a FIFO there blocked `amq init` while it held the lock.
func TestWriteConfigRefusesAFIFOWithoutBlocking(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootDir, "meta"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rootDir, "meta", "config.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- WriteConfig(path, Config{Version: 1, Agents: []string{"a"}}, false) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("WriteConfig replaced a FIFO without --force")
		}
	case <-time.After(3 * time.Second):
		// Unblock the stuck reader so the goroutine ends.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		} else if !errors.Is(err, syscall.ENXIO) {
			t.Log(err)
		}
		t.Fatal("WriteConfig blocked on a FIFO at config.json")
	}
}
