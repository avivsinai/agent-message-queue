package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileAdapterInjectsPayloads(t *testing.T) {
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "inbox.txt")
	file := File{}

	if err := file.Probe(ctx, target); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if err := file.Inject(ctx, target, "first"); err != nil {
		t.Fatalf("Inject(first) error = %v", err)
	}
	if err := file.Inject(ctx, target, "second"); err != nil {
		t.Fatalf("Inject(second) error = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(data), "first\nsecond\n"; got != want {
		t.Fatalf("payloads = %q, want %q", got, want)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %v, want 0600", got)
	}
}

func TestFileAdapterRejectsDirectoryTarget(t *testing.T) {
	ctx := context.Background()
	target := t.TempDir()
	if err := (File{}).Probe(ctx, target); err == nil {
		t.Fatal("Probe(directory) error = nil, want error")
	}
}
