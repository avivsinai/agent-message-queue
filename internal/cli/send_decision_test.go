package cli

import (
	"strings"
	"testing"
)

// A one-way door always waits for the owner: send refuses door one-way with
// blocking false, and refuses decided on a one-way door — an agent cannot
// decide a one-way door. A record (decided) ships as kind decision, low
// priority, passive: it asks for nothing and starts no turn, so an explicit
// --notify wake on a record is refused, not downgraded.
func TestSendRefusesOneWayDoorRulesAndShapesRecords(t *testing.T) {
	root := t.TempDir()
	configureSendTestRoot(t, root, "lead", "user")
	const oneway = `{"decision":{"options":[{"label":"Go"},{"label":"Not yet"}],"door":"one-way","blocking":true}}`
	const onewayOpen = `{"decision":{"options":[{"label":"Go"},{"label":"Not yet"}],"door":"one-way","blocking":false}}`
	const decidedOneWay = `{"decision":{"options":[{"label":"Go"},{"label":"Not yet"}],"door":"one-way","blocking":true,"decided":"Go"}}`
	const record = `{"decision":{"options":[{"label":"Three retries"},{"label":"One retry"}],"decided":"Three retries","blocking":false}}`
	for _, tc := range []struct{ name, context, kind, notify, want string }{
		{"one-way door with blocking false", onewayOpen, "question", "", "context.decision.blocking"},
		{"decided on a one-way door", decidedOneWay, "question", "", "context.decision.decided"},
		{"record as a question", record, "question", "", "--kind decision"},
		{"record with wake delivery", record, "decision", "wake", "--notify wake"},
		{"valid record", record, "decision", "passive", ""},
		{"valid one-way question", oneway, "question", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--root", root, "--me", "lead", "--to", "user", "--subject", "guard", "--body", "The options.", "--priority", "normal", "--context", tc.context, "--kind", tc.kind}
			if tc.notify != "" {
				args = append(args, "--notify", tc.notify)
			}
			err := runSend(args)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid send refused: %v", err)
				}
				return
			}
			if GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (exit %d), want a usage error naming %q", err, GetExitCode(err), tc.want)
			}
		})
	}
}
