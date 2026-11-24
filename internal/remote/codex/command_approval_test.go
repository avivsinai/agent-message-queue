package codex

import "testing"

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
