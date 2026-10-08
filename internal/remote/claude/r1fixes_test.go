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
	home, installed := installSettings(t, "")
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
	// A second install of the same hook is a no-op.
	if err := InstallStopHook(home, "/opt/amq bin/amq-remote"); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(settingsPath(home)); string(again) != installed {
		t.Fatalf("second install rewrote settings.json:\n%s", again)
	}
}

// codex #855 r1 item 5 and r2 item 3: install into existing hooks without
// Stop, into an operator's existing empty Stop array, and in front of an
// operator's own Stop hook. Install yields valid JSON with one hooks object
// and the prior hooks intact. Uninstall removes our hook and never an
// operator container: a file with its own Stop array round-trips byte for
// byte, including a 2^53 number a float decode would corrupt; the other
// keeps the Stop array we had to create, empty.
func TestInstallIntoExistingHooksRoundTrips(t *testing.T) {
	operatorStop := "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"type\": \"command\",\n        \"command\": \"echo unrelated\"\n      }\n    ]\n  },\n  \"precision_fixture\": 9007199254740993\n}\n"
	for name, tc := range map[string]struct {
		original, afterUninstall string
		stopGroups               int
	}{
		"hooks without Stop": {
			original:       "{\n  \"hooks\": {\n    \"PreToolUse\": [{\"matcher\": \"*\", \"hooks\": [{\"type\": \"command\", \"command\": \"echo pre\"}]}]\n  }\n}\n",
			afterUninstall: "{\n  \"hooks\": {\"Stop\":[],\n    \"PreToolUse\": [{\"matcher\": \"*\", \"hooks\": [{\"type\": \"command\", \"command\": \"echo pre\"}]}]\n  }\n}\n",
			stopGroups:     1,
		},
		"operator's empty Stop": {
			original:       "{\"hooks\":{\"Stop\":[]},\"x\":1}\n",
			afterUninstall: "{\"hooks\":{\"Stop\":[]},\"x\":1}\n",
			stopGroups:     1,
		},
		"operator's Stop hook": {original: operatorStop, afterUninstall: operatorStop, stopGroups: 2},
	} {
		t.Run(name, func(t *testing.T) {
			home, installed := installSettings(t, tc.original)
			if !json.Valid([]byte(installed)) {
				t.Fatalf("install produced invalid JSON:\n%s", installed)
			}
			if n := strings.Count(installed, `"hooks": {`) + strings.Count(installed, `"hooks":{`); n != 1 {
				t.Fatalf("install produced %d top-level hooks objects:\n%s", n, installed)
			}
			var s settingsShape
			_ = json.Unmarshal([]byte(installed), &s)
			if len(s.Hooks["Stop"]) != tc.stopGroups {
				t.Fatalf("Stop chain has %d groups, want %d:\n%s", len(s.Hooks["Stop"]), tc.stopGroups, installed)
			}
			if strings.Contains(tc.original, "PreToolUse") && len(s.Hooks["PreToolUse"]) != 1 {
				t.Fatalf("prior PreToolUse hooks lost:\n%s", installed)
			}
			if err := UninstallStopHook(home); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(settingsPath(home))
			if string(got) != tc.afterUninstall {
				t.Fatalf("after uninstall:\nwant %q\ngot  %q", tc.afterUninstall, got)
			}
		})
	}
}

// codex #855 r2 item 4: an operator added a foreign hook to the matcher
// group we installed. Uninstall removes our inner hook and keeps theirs.
func TestUninstallFromMixedGroupKeepsForeignHook(t *testing.T) {
	home, _ := installSettings(t, "")
	raw, _ := os.ReadFile(settingsPath(home))
	mixed := strings.Replace(string(raw), `,"timeout":10}]`, `,"timeout":10},{"type":"command","command":"echo operator"}]`, 1)
	if mixed == string(raw) {
		t.Fatalf("setup: could not add a foreign hook to:\n%s", raw)
	}
	if err := os.WriteFile(settingsPath(home), []byte(mixed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UninstallStopHook(home); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(settingsPath(home))
	var s settingsShape
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("uninstall produced invalid JSON: %v\n%s", err, got)
	}
	if strings.Contains(string(got), stopHookMarker) {
		t.Fatalf("our hook is still installed:\n%s", got)
	}
	if g := s.Hooks["Stop"]; len(g) != 1 || len(g[0].Hooks) != 1 || g[0].Hooks[0].Command != "echo operator" {
		t.Fatalf("foreign hook not preserved: %+v\n%s", g, got)
	}
}

// codex #855 r1 receiver boundary: a session id with separators must not
// become a path outside the marker directory, even when a binding names it.
func TestReceiverRefusesTraversalSessionID(t *testing.T) {
	home := t.TempDir()
	bindingPath := filepath.Join(home, "binding.json")
	if err := os.WriteFile(bindingPath, []byte(`{"native_session":"../escape"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMQ_REMOTE_BINDING", bindingPath)
	if code := RunStopHookReceiver(home, strings.NewReader(`{"session_id":"../escape"}`), os.Stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(claudeSessionsDir(home), "escape.jsonl")); err == nil {
		t.Fatal("receiver wrote outside the amq-stop directory")
	}
}
