package protocol

import (
	"strings"
	"testing"
)

// TestBoundReasonRendersInertInDM pins bead agent-message-queue-lfd: adapter
// refusal text cannot carry direction overrides, line breaks or zero-width
// characters into the Buzz DM. Filtering alone cannot make text inert — the
// DM renderer decodes entities and autolinks URLs (review of #972 r3) — so
// inertness of the rest is the rendering boundary's job (code spans in
// internal/acp); these rows pin what BoundReason itself removes.
func TestBoundReasonRendersInertInDM(t *testing.T) {
	in := "bridge \u202ereversed refused\u2028invisible\u200bend\u2029override"
	out := BoundReason(in)
	for _, bad := range []string{"\u202e", "\u2028", "\u2029", "\u200b"} {
		if strings.Contains(out, bad) {
			t.Fatalf("BoundReason kept %q in %q", bad, out)
		}
	}
	if !strings.Contains(out, "bridge") || !strings.Contains(out, "refused") || !strings.Contains(out, "override") {
		t.Fatalf("BoundReason dropped the visible words: %q", out)
	}
	if strings.ContainsAny(out, "\n\r") {
		t.Fatalf("BoundReason kept a line break: %q", out)
	}
}

// TestBoundReasonKeepsOrdinaryText is the counterpart regression: the
// sanitization must not mangle normal adapter text. Identifiers with
// underscores, paths, parens, e-mail addresses, Markdown-ish punctuation
// and URLs are common in refusal reasons and must survive unchanged; the
// DM's rendering boundary makes them inert, not this filter.
func TestBoundReasonKeepsOrdinaryText(t *testing.T) {
	for _, in := range []string{
		"turn_in_progress",
		"CODEX_HOME is unset",
		"exit status 1 (permission denied)",
		"~/.codex/config.toml not found",
		"notify user@host.example failed",
		"see [click here](https://evil.example) or @everyone",
		"&#x202E;abc&#x202C;",
	} {
		if out := BoundReason(in); out != in {
			t.Fatalf("BoundReason(%q) = %q; want it unchanged", in, out)
		}
	}
}
