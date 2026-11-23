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

func TestCommandApprovalStaysLocalWithoutCompleteText(t *testing.T) {
	for name, params := range map[string]string{
		"unknown field":    strings.Replace(plainCommandApprovalParams, `"cwd":`, `"newField":true,"cwd":`, 1),
		"cut command":      strings.Replace(plainCommandApprovalParams, `"command":"ls"`, `"command":"`+strings.Repeat("x", maxDialogText)+`"`, 1),
		"missing item":     strings.Replace(plainCommandApprovalParams, `"itemId":"i1",`, "", 1),
		"write stdin":      strings.Replace(plainCommandApprovalParams, `"kind":"command"`, `"kind":"writeStdin"`, 1),
		"object command":   strings.Replace(plainCommandApprovalParams, `"command":"ls"`, `"command":{"command":"ls"}`, 1),
		"environment":      strings.Replace(plainCommandApprovalParams, `"environmentId":null`, `"environmentId":"other"`, 1),
		"permissions":      strings.Replace(plainCommandApprovalParams, `"cwd":`, `"additionalPermissions":{},"cwd":`, 1),
		"network":          strings.Replace(plainCommandApprovalParams, `"cwd":`, `"networkApprovalContext":{"protocol":"https","host":"example.org"},"cwd":`, 1),
		"policy amendment": strings.Replace(plainCommandApprovalParams, `"cwd":`, `"proposedExecpolicyAmendment":["ls"],"cwd":`, 1),
		"implicit choices": strings.Replace(plainCommandApprovalParams, `["acceptForSession","decline","cancel","accept"]`, "null", 1),
	} {
		t.Run(name, func(t *testing.T) {
			a := &Attachment{threadID: "t1"}
			d := a.dialogFor(ServerRequest{ID: json.RawMessage(`"s"`), Method: methodCommandApproval, Params: json.RawMessage(params)})
			if d == nil || d.req.Complete || d.req.Text == "" {
				t.Fatalf("local command dialog = %+v", d)
			}
		})
	}
}
