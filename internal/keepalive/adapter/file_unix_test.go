//go:build unix

package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFileAdapterInjectRefusesFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- (File{}).Inject(ctx, path, "payload")
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrTargetNotRegular) {
			t.Fatalf("Inject(FIFO) error = %v, want %v", err, ErrTargetNotRegular)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Inject blocked on FIFO")
	}
}

func TestFileAdapterInjectRefusesDevice(t *testing.T) {
	err := (File{}).Inject(context.Background(), "/dev/null", "payload")
	if !errors.Is(err, ErrTargetNotRegular) {
		t.Fatalf("Inject(/dev/null) error = %v, want %v", err, ErrTargetNotRegular)
	}
}

func TestFileAdapterInjectRefusesSymlinkSwapAfterProbe(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(victim): %v", err)
	}
	target := filepath.Join(dir, "inbox.txt")
	file := File{}
	if err := file.Probe(context.Background(), target); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if err := os.Symlink(victim, target); err != nil {
		t.Fatalf("Symlink(target): %v", err)
	}
	if err := file.Inject(context.Background(), target, "pwned"); err == nil {
		t.Fatal("Inject(symlink) error = nil, want refuse")
	} else if !errors.Is(err, ErrTargetSymlink) {
		t.Fatalf("Inject(symlink) error = %v, want %v", err, ErrTargetSymlink)
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile(victim): %v", err)
	}
	if got, want := string(data), "keep\n"; got != want {
		t.Fatalf("victim bytes = %q, want %q", got, want)
	}
}
