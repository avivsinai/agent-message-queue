//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The service review found that a restart loop wrote its earliest route
// error outside rotation, especially after the project's store was rerouted.
func TestWatchStartupFailureRotatesInheritedLog(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("PATH", dir)
	logDir := filepath.Join(dir, "old-store", "extensions", "board")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "watch.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	const limit = 1 << 20
	if err := f.Truncate(2 * limit); err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = previous })
	if code := runSync([]string{"--watch"}); code != exitGeneral {
		t.Fatalf("exit %d", code)
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > limit || !strings.Contains(string(raw), "amq") {
		t.Fatalf("startup log size %d, err %v", len(raw), err)
	}
	old, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if old.Size() != limit {
		t.Fatalf("archived %d bytes, want %d", old.Size(), limit)
	}
}
