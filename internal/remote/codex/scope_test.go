package codex

import (
	"encoding/json"
	"strings"
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
	for i, scope := range []string{dialog.ScopeSession, dialog.ScopeOnce, dialog.ScopeOnce} {
		if options[i].Scope != scope {
			t.Fatalf("option %d scope = %q; want %q", i+1, options[i].Scope, scope)
		}
	}
	if !options[2].Deny {
		t.Fatal("decline is not marked a deny option")
	}
	if answer, ok := dialog.ParseAnswer(dialog.Request{Options: options}, "yes"); !ok || answer.Option != 2 {
		t.Fatalf("yes = %+v, %v; want once", answer, ok)
	}
	amendment, _ := decisions([]json.RawMessage{json.RawMessage(`{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["git","push"]}}`)})
	if len(amendment) == 0 || amendment[0].Scope != dialog.ScopeAlways {
		t.Fatalf("amendment scope = %+v", amendment)
	}
	permissions, _ := permissionChoices(json.RawMessage(`{}`))
	if permissions[0].Scope != dialog.ScopeTurn || permissions[1].Scope != dialog.ScopeSession || permissions[2].Scope != dialog.ScopeOnce || !permissions[2].Deny {
		t.Fatalf("permission scopes = %+v", permissions)
	}
	elicitation, _, _ := elicitationChoices("form", nil)
	if elicitation[0].Scope != dialog.ScopeOnce || elicitation[1].Scope != dialog.ScopeOnce || elicitation[2].Scope != "" || !elicitation[1].Deny {
		t.Fatalf("elicitation scopes = %+v", elicitation)
	}
}

// R6: only a command approval whose text was not cut is marked complete, and
// the native turn rides the dialog record for whatever follows.
func TestDialogForMarksAWholeCommandAndNamesTheTurn(t *testing.T) {
	a := &Attachment{threadID: "t1"}
	d := a.dialogFor(ServerRequest{ID: json.RawMessage(`"s1"`), Method: methodCommandApproval,
		Params: json.RawMessage(`{"threadId":"t1","turnId":"turn-9","kind":"command","command":"rm -rf node_modules","cwd":"/w"}`)})
	if d == nil || d.req.Turn != "turn-9" || d.turnID != "turn-9" || d.req.Complete {
		t.Fatalf("command dialog = %+v", d.req)
	}
	d = a.dialogFor(ServerRequest{ID: json.RawMessage(`"s2"`), Method: methodFileChangeApproval,
		Params: json.RawMessage(`{"threadId":"t1","turnId":"turn-9"}`)})
	if d == nil || d.req.Complete {
		t.Fatalf("file-change dialog marked complete: %+v", d.req)
	}
	d = a.dialogFor(ServerRequest{ID: json.RawMessage(`"s3"`), Method: methodCommandApproval,
		Params: json.RawMessage(`{"threadId":"t1","turnId":"turn-9","command":"` + strings.Repeat("x", maxDialogText+1) + `"}`)})
	if d == nil || d.req.Complete {
		t.Fatalf("cut command dialog marked complete: %+v", d.req)
	}
}

func TestDialogForCompletenessCoversTheRenderedDecisionOnly(t *testing.T) {
	a := &Attachment{threadID: "t1"}
	// No codex command approval is marked complete (review of ffe44020):
	// the 0.171.0 native schema requires itemId and startedAtMs in every
	// command approval, so the params never reduce to a fully rendered
	// shape, and the optional decision fields render incompletely. The
	// command text is still rendered for the owner.
	for name, params := range map[string]string{
		"plain shape":    `{"threadId":"t1","turnId":"u","itemId":"i1","startedAtMs":1,"kind":"command","command":"ls","cwd":"/w","availableDecisions":["accept","decline"]}`,
		"minimal shape":  `{"threadId":"t1","turnId":"u","kind":"command","command":"ls"}`,
		"writeStdin":     `{"threadId":"t1","turnId":"u","itemId":"i1","startedAtMs":1,"kind":"writeStdin","command":"cat"}`,
		"object command": `{"threadId":"t1","turnId":"u","itemId":"i1","startedAtMs":1,"kind":"command","command":{"command":"ls"}}`,
		"env id":         `{"threadId":"t1","turnId":"u","itemId":"i1","startedAtMs":1,"kind":"command","command":"ls","environmentId":"env-7"}`,
	} {
		d := a.dialogFor(ServerRequest{ID: json.RawMessage(`"s"`), Method: methodCommandApproval,
			Params: json.RawMessage(params)})
		if d == nil || d.req.Complete {
			t.Fatalf("%s marked complete: %+v", name, d)
		}
	}
}
