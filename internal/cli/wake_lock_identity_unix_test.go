//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCanonicalizeWakeRootFailsClosedOnELOOPAndEACCES(t *testing.T) {
	dir := t.TempDir()
	loopA := filepath.Join(dir, "loop-a")
	loopB := filepath.Join(dir, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatalf("Symlink A: %v", err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatalf("Symlink B: %v", err)
	}
	got, err := canonicalizeWakeRoot(loopA)
	if err == nil || got != "" {
		t.Fatalf("canonicalizeWakeRoot(ELOOP) = %q, %v; want error not identity", got, err)
	}
	if !errors.Is(err, syscall.ELOOP) && !strings.Contains(strings.ToLower(err.Error()), "too many links") {
		t.Fatalf("canonicalizeWakeRoot(ELOOP) error = %v, want ELOOP", err)
	}
	if canonicalWakeRoot(loopA) != "" {
		t.Fatalf("canonicalWakeRoot(ELOOP) = %q, want empty rather than lexical identity", canonicalWakeRoot(loopA))
	}

	denied := filepath.Join(dir, "denied")
	if err := os.Mkdir(denied, 0o700); err != nil {
		t.Fatalf("Mkdir denied: %v", err)
	}
	child := filepath.Join(denied, "root")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("Mkdir child: %v", err)
	}
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatalf("Chmod denied: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o700) })
	got, err = canonicalizeWakeRoot(child)
	_ = os.Chmod(denied, 0o700)
	if err == nil || got != "" {
		t.Fatalf("canonicalizeWakeRoot(EACCES) = %q, %v; want error not identity", got, err)
	}
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, os.ErrPermission) {
		t.Fatalf("canonicalizeWakeRoot(EACCES) error = %v, want EACCES", err)
	}
}
