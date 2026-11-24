package codex

import (
	"encoding/json"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// Source: live run 2026-11-24, Codex CLI command dialog of a link task.
// Codex proposed an exec-policy amendment for an ordinary command.
const liveAmendmentParams = `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"kind":"command",
"command":"/bin/zsh -lc \"/usr/bin/printf 'X'\"","cwd":"/w","reason":"Allow this command to run once?",
"commandActions":[],
"proposedExecpolicyAmendment":["/usr/bin/printf","X"],
"availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["/usr/bin/printf","X"]}},"cancel"]}`

func TestPlainCommandTextAcceptsProposedAmendment(t *testing.T) {
	want := "$ /bin/zsh -lc \"/usr/bin/printf 'X'\"\nin /w\nreason: Allow this command to run once?"
	if got := plainCommandText([]byte(liveAmendmentParams)); got != want {
		t.Fatalf("plainCommandText = %q, want %q", got, want)
	}
}

func TestPlainCommandTextRefusesUnmatchedAmendment(t *testing.T) {
	noProposal := `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"kind":"command","command":"c","cwd":"/w","reason":"r","commandActions":[],
"availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["a"]}}]}`
	differs := `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"kind":"command","command":"c","cwd":"/w","reason":"r","commandActions":[],
"proposedExecpolicyAmendment":["a"],
"availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["b"]}}]}`
	unoffered := `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"kind":"command","command":"c","cwd":"/w","reason":"r","commandActions":[],
"proposedExecpolicyAmendment":["a"],
"availableDecisions":["accept","cancel"]}`
	base := func(proposal, decision string) string {
		return `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"kind":"command","command":"c","cwd":"/w","reason":"r","commandActions":[],
"proposedExecpolicyAmendment":` + proposal + `,"availableDecisions":["accept",` + decision + `]}`
	}
	for name, raw := range map[string]string{
		"no proposal": noProposal, "differs": differs, "proposal with no decision": unoffered,
		"null element":  base(`["a",null]`, `{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["a",null]}}`),
		"not an array":  base(`"a"`, `{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":"a"}}`),
		"repeated key":  base(`["a"]`, `{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["b"],"execpolicy_amendment":["a"]}}`),
		"extra key":     base(`["a"]`, `{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["a"]},"other":1}`),
		"missing field": base(`["a"]`, `{"acceptWithExecpolicyAmendment":{}}`),
	} {
		if got := plainCommandText([]byte(raw)); got != "" {
			t.Errorf("%s: plainCommandText = %q, want empty", name, got)
		}
	}
}

// Source: three command-approval requests captured from a Codex 0.172.0
// app-server on 2026-11-24 (thread, turn and item ids and the directory are
// replaced, and the keys are sorted; every value is as sent). The 0.170.2 release
// withheld each one from the link: environmentId is "local", not null.
var capturedCommandApprovals = map[string]string{
	"inferred amendment":         `{"availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["/usr/bin/printf","X\\n"]}},"cancel"],"command":"/bin/zsh -lc \"/usr/bin/printf 'X\\\\n'\"","commandActions":[{"command":"/usr/bin/printf 'X\\n'","type":"unknown"}],"cwd":"/w","environmentId":"local","itemId":"i1","kind":"command","proposedExecpolicyAmendment":["/usr/bin/printf","X\\n"],"reason":"Do you approve running /usr/bin/printf 'X\\n' once?","startedAtMs":1795545048324,"threadId":"t1","turnId":"turn-1"}`,
	"prefix rule amendment":      `{"availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["/usr/bin/printf"]}},"cancel"],"command":"/bin/zsh -lc \"/usr/bin/printf 'X\\\\n'\"","commandActions":[{"command":"/usr/bin/printf 'X\\n'","type":"unknown"}],"cwd":"/w","environmentId":"local","itemId":"i1","kind":"command","proposedExecpolicyAmendment":["/usr/bin/printf"],"reason":"Do you want to allow this command to print X followed by a newline?","startedAtMs":1795545053297,"threadId":"t1","turnId":"turn-1"}`,
	"ls with a listFiles action": `{"availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["ls"]}},"cancel"],"command":"/bin/zsh -lc ls","commandActions":[{"command":"ls","path":null,"type":"listFiles"}],"cwd":"/w","environmentId":"local","itemId":"i1","kind":"command","proposedExecpolicyAmendment":["ls"],"reason":"Do you want to allow this single invocation of ls in the current directory?","startedAtMs":1795545058749,"threadId":"t1","turnId":"turn-1"}`,
}

func TestCapturedLocalCommandApprovalIsCompleteForTheLink(t *testing.T) {
	for name, raw := range capturedCommandApprovals {
		a := &Attachment{threadID: "t1"}
		d := a.dialogFor(ServerRequest{ID: json.RawMessage(`"s"`), Method: methodCommandApproval, Params: json.RawMessage(raw)})
		if d == nil || !d.req.Complete || d.req.Turn != "turn-1" {
			t.Errorf("%s: dialog = %+v; want a complete dialog of turn-1", name, d)
			continue
		}
		in, ok := dialog.RenderLinkInteraction(d.req)
		if !ok || len(in.Options) != 1 || in.Options[0] != "Allow once" {
			t.Errorf("%s: link interaction = %+v, %v; want the one choice Allow once", name, in, ok)
		}
	}
}
