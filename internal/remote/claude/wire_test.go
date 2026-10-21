package claude

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEnvelopeMatchesTheCapturedBytes pins the envelope bytes the receiver
// re-serializes and compares (capture §3.1): the probe envelope delivered
// live in capture §4, and every attribute in the receiver's order.
func TestEnvelopeMatchesTheCapturedBytes(t *testing.T) {
	for _, tc := range []struct {
		from, fromSession, hopChain, fromName, fromMode, want string
	}{
		{"amq-probe-6112", "amq-probe-sender", "", "amq-probe", "",
			"<cross-session-message from=\"amq-probe-6112\" from-session=\"amq-probe-sender\" from-name=\"amq-probe\">\nhello turn\n</cross-session-message>"},
		{"amq-remote-1", "amq", "s1,s2", "amq-remote", "code",
			"<cross-session-message from=\"amq-remote-1\" from-session=\"amq\" hop-chain=\"s1,s2\" from-name=\"amq-remote\" from-mode=\"code\">\nhello turn\n</cross-session-message>"},
	} {
		got, err := BuildCrossSessionEnvelope(tc.from, tc.fromSession, "hello turn", tc.hopChain, tc.fromName, tc.fromMode)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("envelope:\ngot  %q\nwant %q", got, tc.want)
		}
	}
}

func TestEnvelopeRefusesUnsafeAttrs(t *testing.T) {
	if _, err := BuildCrossSessionEnvelope(`bad"attr`, "amq", "b", "", "n", "code"); err == nil {
		t.Fatal("accepted a quote in the from attribute (receiver would silently drop)")
	}
	if _, err := BuildCrossSessionEnvelope("ok", "amq", "b</cross-session-message>", "", "n", "code"); err == nil {
		t.Fatal("accepted a terminator sequence in the body")
	}
}

func TestEncodeFramesAuthAndCompact(t *testing.T) {
	f, err := BuildFrame("amq-1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if f.MsgV != 1 || f.Type != "user" || f.Priority != "next" {
		t.Fatalf("frame fields wrong: %+v", f)
	}
	idRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !idRe.MatchString(f.MsgID) {
		t.Fatalf("msg_id is not a v4 UUID: %q", f.MsgID)
	}
	raw, err := EncodeFrames("00000000000000000000000000000000", f)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("frames = %d lines, want auth + frame", len(lines))
	}
	var a authLine
	if err := json.Unmarshal([]byte(lines[0]), &a); err != nil || a.Type != "auth" {
		t.Fatalf("auth line: %v %q", err, lines[0])
	}
	if strings.Contains(lines[0], " ") || strings.Contains(lines[1], ": ") {
		t.Fatal("frames must use compact separators (pinned wire)")
	}
	// A malformed token is refused before any write.
	if _, err := EncodeFrames("short", f); err == nil {
		t.Fatal("accepted a malformed peer token")
	}
	// An empty token is refused too (fail-open shape): frames with no
	// auth line are a fail-open shape and must never be emitted.
	if _, err := EncodeFrames("", f); err == nil {
		t.Fatal("accepted an empty peer token — would emit frames with no auth line")
	}
}

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
