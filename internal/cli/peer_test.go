package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeerPairWritesNothingUntilFingerprintIsSet(t *testing.T) {
	called := false
	old := lookAmqBridge
	lookAmqBridge = func() (string, error) {
		called = true
		return "/bin/false", nil
	}
	t.Cleanup(func() { lookAmqBridge = old })

	err := runPeerPair([]string{"--root", t.TempDir(), "amq1:box:host=box generation=1 public=aa;x25519=" + strings.Repeat("ab", 32)})
	if err == nil {
		t.Fatal("peer pair without confirm = nil")
	}
	if !strings.Contains(err.Error(), "confirm-fingerprint") {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("looked up amq-bridge before confirm")
	}
}

func TestPeerPairExecsAddThenLine(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "amq-bridge")
	body := "#!/bin/sh\nprintf '%s\\n' \"$0\" \"$@\" >>\"$AMQ_PAIR_LOG\"\nif [ \"$1\" = peer ] && [ \"$2\" = line ]; then printf 'amq1:mac:local\\n'; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "log")
	t.Setenv("AMQ_PAIR_LOG", logPath)
	old := lookAmqBridge
	lookAmqBridge = func() (string, error) { return script, nil }
	t.Cleanup(func() { lookAmqBridge = old })

	fp := strings.Repeat("a", 26)
	line := "amq1:box:host=box generation=1 public=aa;x25519=" + strings.Repeat("ab", 32)
	if err := runPeerPair([]string{"--root", dir, "--confirm-fingerprint", fp, "--allow-in", "codex", line}); err != nil {
		t.Fatalf("runPeerPair: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "peer\nadd\n") || !strings.Contains(text, "--confirm-fingerprint\n"+fp+"\n") || !strings.Contains(text, "peer\nline\n") {
		t.Fatalf("bridge argv log = %q", text)
	}
}
