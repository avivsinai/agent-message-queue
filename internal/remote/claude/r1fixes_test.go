package claude

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codex #855 r1 item 1: the key-file hash is over Node path.resolve, which
// is lexical. A socket path through a symlinked directory (macOS /tmp ->
// /private/tmp) must hash the path as given, not its resolved form.
// Reproduced live: submit to pid 52416 was refused "no inbound".
func TestPeerKeyHashIsLexical(t *testing.T) {
	home := tempHome(t, 7, &sessionRegistry{Pid: 7, SessionID: "s7", Kind: "interactive"})
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "7.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(link, "7.sock")
	sum := sha256.Sum256([]byte(sock))
	keyFile := filepath.Join(claudeSessionsDir(home), fmt.Sprintf("7.%x.key", sum))
	if err := os.WriteFile(keyFile, []byte(`{"peerToken":"00000000000000000000000000000000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := readPeerToken(home, 7, sock); err != nil || tok != "00000000000000000000000000000000" {
		t.Fatalf("token=%q err=%v; the key minted under the lexical path was not found", tok, err)
	}
}

func installSettings(t *testing.T, original string) (home string, installed string) {
	t.Helper()
	home = t.TempDir()
	if original != "" {
		if err := os.MkdirAll(filepath.Dir(settingsPath(home)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(settingsPath(home), []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := InstallStopHook(home, "/opt/amq bin/amq-remote"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(settingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	return home, string(raw)
}

// settingsShape decodes the Stop chain in Claude Code's schema:
// hooks.Stop[] is a list of matcher groups, each with a hooks[] list.
type settingsShape struct {
	Hooks map[string][]struct {
		Hooks []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// codex #855 r1 items 3 and 4: the installed hook is a matcher group whose
// nested command runs the quoted amq-remote binary (the marker is an env
// assignment in front of it, not the command word).
func TestInstalledStopHookShapeAndCommandWord(t *testing.T) {
	_, installed := installSettings(t, "")
	var s settingsShape
	if err := json.Unmarshal([]byte(installed), &s); err != nil {
		t.Fatalf("installed settings is not valid JSON: %v\n%s", err, installed)
	}
	groups := s.Hooks["Stop"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 {
		t.Fatalf("Stop chain = %+v, want one group with one hook", groups)
	}
	cmd := groups[0].Hooks[0].Command
	want := stopHookMarker + `'/opt/amq bin/amq-remote' claude stop-hook`
	if cmd != want {
		t.Fatalf("command = %q, want %q", cmd, want)
	}
}

// codex #855 r1 item 5: existing hooks without Stop, and an existing empty
// Stop array. Install yields valid JSON with one hooks key and the prior
// hooks intact; uninstall restores the original bytes.
func TestInstallIntoExistingHooksRoundTrips(t *testing.T) {
	for name, original := range map[string]string{
		"hooks without Stop": "{\n  \"hooks\": {\n    \"PreToolUse\": [{\"matcher\": \"*\", \"hooks\": [{\"type\": \"command\", \"command\": \"echo pre\"}]}]\n  }\n}\n",
		"empty Stop":         "{\n  \"hooks\": {\n    \"Stop\": []\n  }\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			home, installed := installSettings(t, original)
			if !json.Valid([]byte(installed)) {
				t.Fatalf("install produced invalid JSON:\n%s", installed)
			}
			if n := strings.Count(installed, `"hooks": {`) + strings.Count(installed, `"hooks":{`); n != 1 {
				t.Fatalf("install produced %d top-level hooks objects:\n%s", n, installed)
			}
			var s settingsShape
			_ = json.Unmarshal([]byte(installed), &s)
			if len(s.Hooks["Stop"]) != 1 {
				t.Fatalf("Stop chain has %d groups, want 1:\n%s", len(s.Hooks["Stop"]), installed)
			}
			if strings.Contains(original, "PreToolUse") && len(s.Hooks["PreToolUse"]) != 1 {
				t.Fatalf("prior PreToolUse hooks lost:\n%s", installed)
			}
			if err := UninstallStopHook(home); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(settingsPath(home))
			if string(got) != original {
				t.Fatalf("uninstall is not byte-for-byte:\nwant %q\ngot  %q", original, got)
			}
		})
	}
}

// codex #855 r1 receiver boundary: a session id with separators must not
// become a path outside the marker directory.
func TestReceiverRefusesTraversalSessionID(t *testing.T) {
	home := t.TempDir()
	if code := RunStopHookReceiver(home, strings.NewReader(`{"session_id":"../escape"}`), os.Stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(claudeSessionsDir(home), "escape.jsonl")); err == nil {
		t.Fatal("receiver wrote outside the amq-stop directory")
	}
}
