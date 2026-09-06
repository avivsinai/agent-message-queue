package adapter

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("file mode = %v, want 0600", got)
	}
}

func TestFileNormalizedTargetStaysBoundAcrossWorkingDirectoryChanges(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	file := File{}
	target, err := file.NormalizeTarget("launchd-inbox.txt")
	if err != nil {
		t.Fatalf("NormalizeTarget() error = %v", err)
	}
	t.Chdir(t.TempDir())
	if err := file.Inject(context.Background(), target, "delivered after launchd cwd change"); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "launchd-inbox.txt"))
	if err != nil {
		t.Fatalf("ReadFile(normalized target): %v", err)
	}
	if got, want := string(data), "delivered after launchd cwd change\n"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}
