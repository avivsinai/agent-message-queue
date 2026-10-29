package claude

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Regression: #855, the key-file hash is over Node path.resolve, which
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
		if err := os.MkdirAll(filepath.Dir(SettingsPath(home)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(SettingsPath(home), []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := InstallStopHook(home, "/opt/amq bin/amq-remote"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(SettingsPath(home))
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
