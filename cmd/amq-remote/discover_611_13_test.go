package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 611.13: `up --discover` lists the running Claude session with a manifest
// entry to paste, and returns before any supervisor side effect: no
// registry directory, no lifetime lock.
func TestUpDiscoverListsCandidatesWithoutSupervising(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	reg, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "s1", "kind": "interactive", "messagingSocketPath": "/tmp/cc-socks/x.sock", "name": "main"})
	if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(pid)+".json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	regPath := filepath.Join(root, "supervisor", "registry.json")
	var out, errOut bytes.Buffer
	code, err := up([]string{"--root", root, "--discover", "--registry", regPath}, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("up --discover: code=%d err=%v stderr=%s", code, err, errOut.String())
	}
	want := `{"config":{"pid":` + strconv.Itoa(pid) + `},"kind":"claude","target":"claude:` + strconv.Itoa(pid) + `"}`
	if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "main") {
		t.Fatalf("discover output = %q, want the claude candidate entry %s", out.String(), want)
	}
	if _, err := os.Stat(filepath.Dir(regPath)); !os.IsNotExist(err) {
		t.Fatalf("up --discover touched the supervisor registry dir: %v", err)
	}
}
