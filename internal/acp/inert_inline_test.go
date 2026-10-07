package acp

import (
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Review of #972 r3: character filtering cannot make adapter text inert in
// the DM — the renderer (remark + remark-gfm) decodes HTML entities back
// into bidi overrides and autolinks URLs. inertInline wraps the text in a
// code span, where neither happens.
func TestInertInline(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"entity", "&#x202E;abc", "`&#x202E;abc`"},
		{"url", "see https://evil.example", "`see https://evil.example`"},
		{"mention", "@everyone", "`@everyone`"},
		{"plain backticks", "run `amq status`", "`` run `amq status` ``"},
		{"leading backtick", "`quoted", "`` `quoted ``"},
		{"trailing backtick", "quoted`", "`` quoted` ``"},
		{"backtick run", "a ``` b", "````a ``` b````"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inertInline(tc.in); got != tc.want {
				t.Fatalf("inertInline(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The real DM-text site: a rejected snapshot's status renders the adapter's
// reason inside a code span, so an entity-encoded bidi override or a URL in
// the reason stays visible text in the DM body, never an override or link.
func TestStatusTextRendersTheReasonInert(t *testing.T) {
	r := &remoteTurn{meta: remoteMeta{Target: "pi", State: "rejected", Reason: "refused: see &#x202E;this&#x202C; and https://evil.example"}}
	out := r.statusText(protocol.Snapshot{})
	if !strings.Contains(out, "`refused: see &#x202E;this&#x202C; and https://evil.example`") {
		t.Fatalf("statusText did not render the reason as an inert code span: %q", out)
	}
}
