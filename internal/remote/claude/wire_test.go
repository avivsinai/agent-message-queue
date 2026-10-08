package claude

import (
	"encoding/json"
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
	// An empty token is refused too (architect PR2 note): frames with no
	// auth line are a fail-open shape and must never be emitted.
	if _, err := EncodeFrames("", f); err == nil {
		t.Fatal("accepted an empty peer token — would emit frames with no auth line")
	}
}
