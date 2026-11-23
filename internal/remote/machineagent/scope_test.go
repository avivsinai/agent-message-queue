package machineagent

import (
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// wb7p: a check reaction allows one action, not the session choice first.
func TestTypedDialogReactionAndHintUseNarrowestOption(t *testing.T) {
	req := dialog.Request{ID: "d-00000000000000000000000000000000", Tool: "Bash", Text: "$ ls", Options: []dialog.Option{
		{N: 1, Label: "Allow for session", Value: "acceptForSession", Scope: dialog.ScopeSession},
		{N: 2, Label: "Allow once", Value: "accept", Scope: dialog.ScopeOnce},
		{N: 3, Label: "Deny", Value: "decline", Deny: true},
	}}
	if got := reactionOption(req, []buzzop.Reaction{{Emoji: "✅", PubKeys: []string{"owner"}}}, "owner"); got != 2 {
		t.Fatalf("reaction = %d; want 2", got)
	}
	text := dialogText("Codex", req)
	if !strings.Contains(text, "React ✅ = 2, ❌ = Deny, or reply a number / yes / no.") {
		t.Fatalf("hint = %q", text)
	}
	always := dialog.Request{Options: []dialog.Option{{N: 1, Scope: dialog.ScopeAlways}, {N: 2, Deny: true}}}
	if got := reactionOption(always, []buzzop.Reaction{{Emoji: "✅", PubKeys: []string{"owner"}}}, "owner"); got != 0 {
		t.Fatalf("always reaction = %d; want nothing", got)
	}
	if text := dialogText("Codex", always); strings.Contains(text, "✅") || strings.Contains(text, "/ yes") {
		t.Fatalf("always-only hint = %q", text)
	}
}
