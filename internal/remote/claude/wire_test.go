package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// envelopeRe is the pinned structure from capture §3.1.
var envelopeRe = regexp.MustCompile(`(?s)^<cross-session-message[^>]*>\n.*\n</cross-session-message>$`)

// TestEnvelopeRoundTripPinnedShape mirrors the receiver's re-serialize and
// byte-compare (TG): build → parse attributes → re-serialize → byte-equal.
func TestEnvelopeRoundTripPinnedShape(t *testing.T) {
	env, err := BuildCrossSessionEnvelope("amq-remote-1", "amq", "hello turn", "", "amq-remote", "code")
	if err != nil {
		t.Fatal(err)
	}
	if !envelopeRe.MatchString(env) {
		t.Fatalf("envelope does not match the pinned structure:\n%s", env)
	}
	if strings.Count(env, "\n") != 2 {
		t.Fatalf("envelope must contain exactly two newlines (after open, before close), got %d:\n%s", strings.Count(env, "\n"), env)
	}
	// Re-serialize from the parsed attributes and require byte equality.
	openTag := env[:strings.Index(env, "\n")]
	attrs := parseAttrs(t, openTag)
	again, err := BuildCrossSessionEnvelope(attrs["from"], attrs["from-session"], "hello turn", attrs["hop-chain"], attrs["from-name"], attrs["from-mode"])
	if err != nil {
		t.Fatal(err)
	}
	if again != env {
		t.Fatalf("round trip not byte-exact:\nfirst  %q\nagain  %q", env, again)
	}
}

func parseAttrs(t *testing.T, tag string) map[string]string {
	t.Helper()
	re := regexp.MustCompile(`([a-z-]+)="([^"]*)"`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(tag, -1) {
		out[m[1]] = m[2]
	}
	return out
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
	// An empty token is refused too (architect PR2 note): frames with no
	// auth line are a fail-open shape and must never be emitted.
	if _, err := EncodeFrames("", f); err == nil {
		t.Fatal("accepted an empty peer token — would emit frames with no auth line")
	}
}

func TestReadPeerToken(t *testing.T) {
	home := tempHome(t, 31, &sessionRegistry{Pid: 31, SessionID: "s31", Kind: "interactive"})
	sock := "/tmp/cc-socks/31.sock"
	// Compute the same file name the resolver derives.
	name, err := peerKeyFile(home, 31, sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(`{"peerToken":"00000000000000000000000000000000","pidDomain":"darwin"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := readPeerToken(home, 31, sock)
	if err != nil || tok != "00000000000000000000000000000000" {
		t.Fatalf("token=%q err=%v", tok, err)
	}
	// Missing key fails closed.
	if _, err := readPeerToken(home, 32, "/tmp/cc-socks/32.sock"); err == nil {
		t.Fatal("readPeerToken succeeded with no key file (capture §2: fail closed)")
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(filepath.Dir(name), "33.bad.key"), []byte("{}"), 0o600)
	}
}
