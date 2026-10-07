package protocol

import (
	"strings"
	"testing"
)

// TestBoundReasonRendersInertInDM pins bead agent-message-queue-lfd: adapter
// refusal text cannot impersonate AMQ guidance in the Buzz DM. A reason
// carrying bidi overrides (U+202E), line separators (U+2028), a Markdown
// link and an @mention renders as plain, inert text: no direction change, no
// new line, no link, no mention survives.
func TestBoundReasonRendersInertInDM(t *testing.T) {
	in := "bridge \u202ereversed refused\u2028see [click here](https://evil.example) or @everyone\u2029override"
	out := BoundReason(in)
	for _, bad := range []string{"\u202e", "\u2028", "\u2029", "[", "]", "(", ")", "@"} {
		if strings.Contains(out, bad) {
			t.Fatalf("BoundReason kept %q in %q", bad, out)
		}
	}
	if !strings.Contains(out, "bridge") || !strings.Contains(out, "refused") || !strings.Contains(out, "https://evil.example") {
		t.Fatalf("BoundReason dropped the visible words: %q", out)
	}
	if strings.ContainsAny(out, "\n\r") {
		t.Fatalf("BoundReason kept a line break: %q", out)
	}
}
