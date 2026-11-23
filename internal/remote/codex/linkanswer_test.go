package codex

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// A plain command approval travels the link end to end: the app-server
// asks, the attachment opens the dialog, the link records the answer
// through the exclusive dialog write, the attachment sends the decision
// back and resolves the dialog by link — with the record's own answerer
// carried into the resolution.
func linkAnswerCodexChain(t *testing.T, label, role, wantDecision, wantOutcome string) {
	h := newDialogHarness(t, "cx")
	h.srv.sendServerRequest(t, "7", methodCommandApproval, `{"threadId":"t1","turnId":"tui-1","itemId":"c1","command":"go test ./...","cwd":"/w"}`)
	req := h.opened("")
	if err := dialog.WriteLinkAnswer(h.dir, req.ID, label, role, "sha256:doc", "k1"); err != nil {
		t.Fatal(err)
	}
	if got := h.reply("7"); got != wantDecision {
		t.Fatalf("response = %s want %s", got, wantDecision)
	}
	var r dialog.Resolved
	ok := false
	for deadline := time.Now().Add(4 * time.Second); !ok && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		r, ok, _ = dialog.ReadResolved(h.dir, req.ID)
	}
	if !ok || r.By != dialog.ByLink || r.Outcome != wantOutcome || r.Label != label {
		t.Fatalf("resolved = %+v", r)
	}
}

func TestLinkAnswerCodexChainAllow(t *testing.T) {
	linkAnswerCodexChain(t, "Allow once", dialog.RoleApprove, `{"decision":"accept"}`, dialog.OutcomeAllowed)
}

func TestLinkAnswerCodexChainDeny(t *testing.T) {
	linkAnswerCodexChain(t, "Deny", dialog.RoleDeny, `{"decision":"decline"}`, dialog.OutcomeDenied)
}
