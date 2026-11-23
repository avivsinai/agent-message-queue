package codex

import (
	"encoding/json"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// wb7p: Codex supplies its own decision order, so the response is not
// whatever value happened to come first.
func TestDecisionScopesSurviveCodexOrder(t *testing.T) {
	raws := make([]json.RawMessage, 3)
	for i, value := range []string{"acceptForSession", "accept", "decline"} {
		raws[i], _ = json.Marshal(value)
	}
	options, _ := decisions(raws)
	for i, scope := range []string{dialog.ScopeSession, dialog.ScopeOnce, ""} {
		if options[i].Scope != scope {
			t.Fatalf("option %d scope = %q; want %q", i+1, options[i].Scope, scope)
		}
	}
	if answer, ok := dialog.ParseAnswer(dialog.Request{Options: options}, "yes"); !ok || answer.Option != 2 {
		t.Fatalf("yes = %+v, %v; want once", answer, ok)
	}
	amendment, _ := decisions([]json.RawMessage{json.RawMessage(`{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["git","push"]}}`)})
	if len(amendment) == 0 || amendment[0].Scope != dialog.ScopeAlways {
		t.Fatalf("amendment scope = %+v", amendment)
	}
	permissions, _ := permissionChoices(json.RawMessage(`{}`))
	if permissions[0].Scope != dialog.ScopeTurn || permissions[1].Scope != dialog.ScopeSession || permissions[2].Scope != "" {
		t.Fatalf("permission scopes = %+v", permissions)
	}
	elicitation, _, _ := elicitationChoices("form", nil)
	if elicitation[0].Scope != dialog.ScopeOnce || elicitation[1].Scope != "" || elicitation[2].Scope != "" {
		t.Fatalf("elicitation scopes = %+v", elicitation)
	}
}
