package acp

import (
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

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
