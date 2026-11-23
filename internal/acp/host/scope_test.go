package host

import (
	"encoding/json"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// wb7p: allow_always comes first and an approving word must pick allow_once.
func TestHostPermissionScopesKeepAnAlwaysChoiceExplicit(t *testing.T) {
	root := newRoot(t)
	dir := bindRemote(t, root)
	h, err := Open(Config{Root: root, Me: "grok", Cwd: root, Version: "test", Agent: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	h.setSession("s1")
	params := json.RawMessage(`{"sessionId":"s1","toolCall":{"toolCallId":"t1","title":"build"},"options":[{"optionId":"always","name":"Always","kind":"allow_always"},{"optionId":"once","name":"Once","kind":"allow_once"},{"optionId":"reject","name":"Reject","kind":"reject_once"}]}`)
	answer := h.onRequest("session/request_permission", params)
	done := make(chan any, 1)
	go func() { got, _ := answer(); done <- got }()
	req := waitDialog(t, dir)
	for i, scope := range []string{dialog.ScopeAlways, dialog.ScopeOnce, ""} {
		if req.Options[i].Scope != scope {
			t.Fatalf("option %d = %+v; want scope %q", i+1, req.Options[i], scope)
		}
	}
	got, ok := dialog.ParseAnswer(req, "yes")
	if !ok {
		t.Fatal("yes did not select an option")
	}
	answerDialog(t, dir, req, got.Option)
	var selected map[string]any
	raw, err := json.Marshal(<-done)
	if err != nil || json.Unmarshal(raw, &selected) != nil {
		t.Fatalf("answer = %s (%v)", raw, err)
	}
	outcome := selected["outcome"].(map[string]any)
	if outcome["optionId"] != "once" {
		t.Fatalf("selected = %+v; want once", selected)
	}
}
