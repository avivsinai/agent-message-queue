package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A running serve publishes ~/.amq/board/serve.json, private to the user,
// with what a local client needs; its shutdown removes it.
func TestServeFileIsWrittenPrivateAndRemovedOnShutdown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	want := serveRecord{URL: "http://127.0.0.1:4242", Token: "abc123", PID: os.Getpid(), StartedAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}

	if err := writeServeFile(want); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".amq", "board", "serve.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"url", "token", "pid", "started_at"} {
		if _, ok := fields[k]; !ok {
			t.Fatalf("serve.json lacks %q: %s", k, data)
		}
	}
	var got serveRecord
	if err := json.Unmarshal(data, &got); err != nil || got != want {
		t.Fatalf("serve.json = %+v (%v), want %+v", got, err, want)
	}

	removeServeFile(want.PID)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serve.json still there after shutdown: %v", err)
	}
}
