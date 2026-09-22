package main

import (
	"bytes"
	"encoding/json"
	"net"
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

// codex #858 item 3: a stale Codex daemon socket (a socket file nobody
// listens on) failed discovery as a whole and discarded the healthy Claude
// candidate. The Claude candidate is printed, and the Codex failure is a
// diagnostic on stderr.
func TestDiscoverKeepsClaudeCandidateWhenCodexSocketIsStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeRegistry(t, home)
	sockDir, err := os.MkdirTemp("", "amqd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	stale := filepath.Join(sockDir, "s.sock")
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = l.Close() // leaves the socket file with no listener
	var out, errOut bytes.Buffer
	code, err := up([]string{"--root", t.TempDir(), "--discover", "--codex-socket", stale}, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v stderr=%s", code, err, errOut.String())
	}
	if !strings.Contains(out.String(), `"kind":"claude"`) || !strings.Contains(errOut.String(), "discover codex") {
		t.Fatalf("stdout=%q stderr=%q, want the claude candidate and a codex diagnostic", out.String(), errOut.String())
	}
}

// codex #858 item 2: an explicitly supplied --codex-socket that is not a
// socket is refused as a usage error, not read as "no candidates".
func TestDiscoverRefusesExplicitNonSocket(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	notSock := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(notSock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code, err := up([]string{"--root", t.TempDir(), "--discover", "--codex-socket", notSock}, &out, &errOut)
	if code != 2 || err == nil {
		t.Fatalf("code=%d err=%v, want usage refusal for a non-socket --codex-socket", code, err)
	}
}

func writeClaudeRegistry(t *testing.T, home string) {
	t.Helper()
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	reg, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "s1", "kind": "interactive", "messagingSocketPath": "/tmp/cc-socks/x.sock", "name": "main"})
	if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(pid)+".json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
}
