package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestB42NonEnoentSessionLstatIsTransient verifies that a non-ENOENT Lstat
// failure on the session directory (e.g. EACCES) is classified as
// ErrPeerRootUnreachable (transient, retryable) rather than
// ContextMismatchError (permanent, DLQs the command).
// Bead agent-message-queue-611.22.42.
func TestB42NonEnoentSessionLstatIsTransient(t *testing.T) {
	// chmod(000) is the mechanism to produce a non-ENOENT Lstat failure
	// (EACCES). It cannot revoke directory traversal on Windows (Chmod only
	// toggles the read-only attribute) and is bypassed by privileged POSIX
	// execution (root). Skip on those environments; keep active on normal
	// Linux/macOS CI where the denial is enforceable.
	if runtime.GOOS == "windows" {
		t.Skip("chmod cannot revoke directory traversal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permission denials")
	}

	base := t.TempDir()
	session := "victim"

	// Create the session directory, then remove its parent's read permission
	// so Lstat fails with EACCES (not ENOENT).
	sessionDir := filepath.Join(base, session)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Revoke read+execute on the BASE so Lstat(sessionDir) fails.
	// (On POSIX this gives EACCES; restore before cleanup.)
	if err := os.Chmod(base, 0o000); err != nil {
		t.Fatalf("chmod base: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(base, 0o755)
	})

	_, err := resolveSessionRoot(base, session)
	if err == nil {
		t.Fatal("expected error from resolveSessionRoot, got nil")
	}
	if !errors.Is(err, ErrPeerRootUnreachable) {
		t.Fatalf("expected ErrPeerRootUnreachable, got %T: %v", err, err)
	}

	// Verify retryableRouteError rescues it (the carrier retries, not DLQs).
	routed := retryableRouteError(err)
	if !errors.Is(routed, ErrPeerRootUnreachable) {
		t.Fatalf("retryableRouteError did not preserve ErrPeerRootUnreachable: %v", routed)
	}
}

// TestB42EnoentSessionLstatIsStillNotFound verifies ENOENT stays NotFound
// (not ErrPeerRootUnreachable — the session genuinely doesn't exist yet).
func TestB42EnoentSessionLstatIsStillNotFound(t *testing.T) {
	base := t.TempDir()
	session := "nonexistent"

	_, err := resolveSessionRoot(base, session)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrPeerRootUnreachable) {
		t.Fatalf("ENOENT must not be ErrPeerRootUnreachable (it IS NotFound): %v", err)
	}
	var ecerr *ExitCodeError
	if !errors.As(err, &ecerr) || ecerr.Code != ExitNotFound {
		t.Fatalf("expected ExitNotFound, got %T: %v", err, err)
	}
}
