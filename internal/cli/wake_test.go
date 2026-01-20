//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeTTYForFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// macOS TTYs
		{"/dev/ttys001", "ttys001"},
		{"/dev/ttys123", "ttys123"},
		// Linux PTYs
		{"/dev/pts/1", "pts-1"},
		{"/dev/pts/42", "pts-42"},
		// Edge cases
		{"", "unknown"},
		{"unknown", "unknown"},
		{"/dev/", "unknown"},
		// Unusual but valid
		{"/dev/tty", "tty"},
		{"/dev/console", "console"},
		// Paths with unsafe chars get sanitized
		{"/dev/foo:bar", "foo-bar"},
		{"/dev/foo bar", "foo-bar"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeTTYForFilename(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeTTYForFilename(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPerTTYLockFilenames(t *testing.T) {
	// Verify different TTYs produce different lock filenames
	ttys := []string{
		"/dev/ttys001",
		"/dev/ttys002",
		"/dev/pts/1",
		"/dev/pts/2",
		"",
	}

	seen := make(map[string]string)
	for _, tty := range ttys {
		suffix := sanitizeTTYForFilename(tty)
		lockName := fmt.Sprintf(".wake.%s.lock", suffix)

		if prevTTY, exists := seen[lockName]; exists && tty != "" && prevTTY != "" {
			t.Errorf("TTY %q and %q produce same lock filename %q", prevTTY, tty, lockName)
		}
		seen[lockName] = tty
	}

	// Verify expected lock filenames
	expected := map[string]string{
		"/dev/ttys001": ".wake.ttys001.lock",
		"/dev/pts/1":   ".wake.pts-1.lock",
		"":             ".wake.unknown.lock",
	}
	for tty, wantLock := range expected {
		suffix := sanitizeTTYForFilename(tty)
		gotLock := fmt.Sprintf(".wake.%s.lock", suffix)
		if gotLock != wantLock {
			t.Errorf("TTY %q: got lock %q, want %q", tty, gotLock, wantLock)
		}
	}
}

func TestLegacyLockMigration(t *testing.T) {
	// Create temp directory structure
	tmpDir := t.TempDir()
	agentBase := filepath.Join(tmpDir, "agents", "testuser")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatal(err)
	}

	// Create a legacy .wake.lock with a dead PID
	legacyLock := wakeLock{
		PID:     999999, // Very unlikely to exist
		TTY:     "/dev/ttys999",
		Root:    tmpDir,
		Started: "2026-01-01T00:00:00Z",
	}
	legacyPath := filepath.Join(agentBase, ".wake.lock")
	data, _ := json.Marshal(legacyLock)
	if err := os.WriteFile(legacyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Verify legacy lock exists
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatal("legacy lock should exist before migration")
	}

	// Call acquireWakeLock - it should remove the legacy lock
	// Note: This will fail to acquire (no real TTY in test), but should still migrate
	_, err := acquireWakeLock(tmpDir, "testuser")
	// We expect an error (no TTY available in test environment), but legacy lock should be gone
	_ = err

	// Legacy lock should be removed
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Error("legacy .wake.lock should be removed after migration")
	}
}
