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

func TestFileNormalizeTargetCanonicalizesLexicalRelativeAndSymlinkAliases(t *testing.T) {
	dir := t.TempDir()
	realParent := filepath.Join(dir, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("Mkdir(real parent): %v", err)
	}
	aliasParent := filepath.Join(dir, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatalf("Symlink(alias parent): %v", err)
	}
	t.Chdir(dir)
	file := File{}
	canonical, err := file.NormalizeTarget(filepath.Join("real", "inbox.txt"))
	if err != nil {
		t.Fatalf("NormalizeTarget(canonical) error = %v", err)
	}
	for _, target := range []string{
		filepath.Join(".", "real", "..", "real", "inbox.txt"),
		filepath.Join("alias", "inbox.txt"),
	} {
		got, err := file.NormalizeTarget(target)
		if err != nil {
			t.Fatalf("NormalizeTarget(%q) error = %v", target, err)
		}
		if got != canonical {
			t.Fatalf("NormalizeTarget(%q) = %q, want %q", target, got, canonical)
		}
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

func TestFileNormalizeTargetFailsClosedOnInvalidOrAmbiguousPaths(t *testing.T) {
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatalf("Symlink(loop): %v", err)
	}
	for _, target := range []string{"", dir, filepath.Join(dir, "missing", "inbox.txt"), loop} {
		if _, err := (File{}).NormalizeTarget(target); err == nil {
			t.Fatalf("NormalizeTarget(%q) error = nil, want fail-closed error", target)
		}
	}
}
