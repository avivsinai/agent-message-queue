//go:build unix

package adapter

import (
	"context"
	"errors"
	"io"
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

func TestFileAdapterInjectRefusesSameUIDFIFOWithReader(t *testing.T) {
	dir := t.TempDir()
	name := "inbox.fifo"
	path := filepath.Join(dir, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open FIFO reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	payload := "must-not-land"
	err = (File{}).Inject(context.Background(), path, payload)
	if !errors.Is(err, ErrTargetNotRegular) {
		t.Fatalf("Inject(FIFO) error = %v, want %v", err, ErrTargetNotRegular)
	}

	// Pin the post-openat fstat S_IFREG check: with a reader, write-open
	// succeeds instead of ENXIO, so only fstat can refuse before WriteString.
	err = injectAt(context.Background(), dir, name, payload)
	if !errors.Is(err, ErrTargetNotRegular) {
		t.Fatalf("injectAt(FIFO) error = %v, want %v", err, ErrTargetNotRegular)
	}

	got := make([]byte, 64)
	n, readErr := reader.Read(got)
	if n != 0 {
		t.Fatalf("FIFO received %q, want empty", got[:n])
	}
	// injectAt write-opens then closes without WriteString, so the reader
	// sees EOF rather than EAGAIN. Payload landing is the S_IFREG mutant.
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, syscall.EAGAIN) && !errors.Is(readErr, syscall.EWOULDBLOCK) {
		t.Fatalf("FIFO read error = %v, want empty, EOF, or EAGAIN", readErr)
	}
}

func TestFileAdapterProbeRefusesSymlinkAndFIFO(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "inbox.txt")
	if err := os.WriteFile(regular, []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "inbox.link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := (File{}).Probe(context.Background(), link); !errors.Is(err, ErrTargetSymlink) {
		t.Fatalf("Probe(symlink) error = %v, want %v", err, ErrTargetSymlink)
	}

	fifo := filepath.Join(dir, "inbox.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if err := (File{}).Probe(context.Background(), fifo); !errors.Is(err, ErrTargetNotRegular) {
		t.Fatalf("Probe(FIFO) error = %v, want %v", err, ErrTargetNotRegular)
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
