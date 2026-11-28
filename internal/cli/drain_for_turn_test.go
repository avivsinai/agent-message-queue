package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

type forTurnFixture struct {
	file, id, priority, notify, created, body string
	refs, labels                              []string
}

// forTurnMailbox delivers the fixtures to alice from bob under root. File
// names and creation times run against the turn order, so only the tiers
// order them.
func forTurnMailbox(t *testing.T, root string, fixtures []forTurnFixture) string {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range fixtures {
		data, err := format.Message{Header: format.Header{
			Schema: 1, ID: f.id, From: "bob", To: []string{"alice"}, Thread: "p2p/alice__bob",
			Subject: "subject " + f.id, Created: f.created, Refs: f.refs,
			Notify: f.notify, Priority: f.priority, Kind: format.KindStatus, Labels: f.labels,
		}, Body: f.body}.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := deliverToInboxForTest(t, root, "alice", f.file, data); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A drain for a turn gives urgent mail first, then other wake-eligible mail
// in full, then each passive message as a digest whose read command, run
// from the same shell, prints the whole original; every claimed message
// gets its drained receipt. The shell's own root is the base, so the read
// command must carry the route the drain was given (review of the
// --for-turn drain: the printed read command exited 3 after --session and
// after --root).
func TestDrainForTurnDigestsPassiveMailAfterActionableMail(t *testing.T) {
	long := strings.Repeat("é", 100) + "\n\n" + strings.Repeat("ü", 100) + " tail"
	fixtures := []forTurnFixture{
		{file: "a.md", id: "passive", priority: format.PriorityNormal, notify: format.NotifyPassive, created: "2026-01-01T00:00:00Z", body: long, refs: []string{"r1", "r2"}, labels: []string{"fyi"}},
		{file: "b.md", id: "normal", priority: format.PriorityNormal, created: "2026-01-01T00:00:01Z", body: "normal body"},
		{file: "c.md", id: "urgent", priority: format.PriorityUrgent, created: "2026-01-01T00:00:02Z", body: "urgent body"},
	}
	for _, tc := range []struct {
		name  string
		route func(parent string) (root string, args []string)
	}{
		{"session", func(parent string) (string, []string) {
			return sessionRoot(t, parent, "side", "alice", "bob"), []string{"--session", "side"}
		}},
		{"root outside the shell's tree", func(string) (string, []string) {
			root := t.TempDir()
			return root, []string{"--root", root, "--ignore-session-pin"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			base := filepath.Join(parent, ".agent-mail")
			if err := fsq.EnsureRootDirs(base); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AM_ROOT", base)
			for _, key := range []string{"AM_BASE_ROOT", "AM_SESSION"} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			root, route := tc.route(parent)
			forTurnMailbox(t, root, fixtures)

			out, err := captureStdout(t, func() error {
				return runDrain(append(route, "--me", "alice", "--for-turn", "--json"))
			})
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			var got drainForTurnResult
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("decode %q: %v", out, err)
			}
			if len(got.Drained) != 2 || got.Drained[0].ID != "urgent" || got.Drained[0].Body != "urgent body\n" ||
				got.Drained[1].ID != "normal" || got.Drained[1].Body != "normal body\n" {
				t.Fatalf("drained = %+v", got.Drained)
			}
			if got.Count != 3 || got.Remaining != (forTurnRemaining{}) || len(got.Passive) != 1 || strings.Count(out, "ü") != 58 {
				t.Fatalf("result = %s", out)
			}
			d := got.Passive[0]
			want := strings.Repeat("é", 100) + " " + strings.Repeat("ü", 58) + "…"
			if d.ID != "passive" || d.From != "bob" || d.Thread != "p2p/alice__bob" || d.Kind != format.KindStatus ||
				d.Priority != format.PriorityNormal || d.Subject != "subject passive" ||
				!slices.Equal(d.Refs, []string{"r1", "r2"}) || !slices.Equal(d.Labels, []string{"fyi"}) ||
				d.Preview != want || utf8.RuneCountInString(d.Preview) != 160 || !d.Truncated {
				t.Fatalf("digest = %+v", d)
			}
			argv := strings.Fields(d.Read)
			if len(argv) < 2 || argv[0] != "amq" || strings.ContainsAny(d.Read, `'"\`) {
				t.Fatalf("read command %q is not a plain amq command line", d.Read)
			}
			original, err := captureStdout(t, func() error { return Run(argv[1:], "test") })
			if err != nil || !strings.Contains(original, long) {
				t.Fatalf("%s: output %q, error %v", d.Read, original, err)
			}
			for _, id := range []string{"urgent", "normal", "passive"} {
				receipts, err := receipt.List(root, "alice", receipt.ListFilter{MsgID: id, Stage: receipt.StageDrained})
				if err != nil || len(receipts) != 1 {
					t.Fatalf("drained receipts for %s = %+v, %v", id, receipts, err)
				}
			}
		})
	}
}

// The turn order is applied before --limit: with a limit of two, the batch
// is the urgent message and the older normal one, and the rest stay unread.
func TestDrainForTurnOrdersBeforeTheLimit(t *testing.T) {
	root := forTurnMailbox(t, t.TempDir(), []forTurnFixture{
		{file: "a.md", id: "p1", notify: format.NotifyPassive, created: "2026-01-01T00:00:00Z", body: "p1"},
		{file: "b.md", id: "p2", notify: format.NotifyPassive, created: "2026-01-01T00:00:01Z", body: "p2"},
		{file: "c.md", id: "p3", notify: format.NotifyPassive, created: "2026-01-01T00:00:02Z", body: "p3"},
		{file: "d.md", id: "n1", created: "2026-01-01T00:00:03Z", body: "n1"},
		{file: "e.md", id: "n2", created: "2026-01-01T00:00:04Z", body: "n2"},
		{file: "f.md", id: "u1", priority: format.PriorityUrgent, created: "2026-01-01T00:00:05Z", body: "u1"},
	})

	out, err := captureStdout(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--for-turn", "--limit", "2"})
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	urgent, normal := strings.Index(out, "ID: u1"), strings.Index(out, "ID: n1")
	if urgent < 0 || normal < urgent || strings.Contains(out, "ID: n2") || strings.Contains(out, "ID: p") ||
		!strings.Contains(out, "[AMQ] 2 new message(s) for alice:") ||
		!strings.HasSuffix(out, "[AMQ] Unread: 0 urgent, 1 other wake-eligible, 3 passive. Run amq drain --for-turn again for the actionable mail.\n") {
		t.Fatalf("output:\n%s", out)
	}
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	var unread []string
	for _, e := range entries {
		unread = append(unread, e.Name())
	}
	if !slices.Equal(unread, []string{"a.md", "b.md", "c.md", "e.md"}) {
		t.Fatalf("unread = %v", unread)
	}
}

// Review of the --for-turn drain: four parallel drains of one inbox each
// reported dozens of actionable messages left on an empty inbox, because
// the counts were the first scan minus their own claims. Here another
// reader takes the other files during this drain's claim; the counts must
// come from inbox/new after the claim.
func TestDrainForTurnCountsWhatIsLeftAfterAnotherReaderClaims(t *testing.T) {
	root := forTurnMailbox(t, t.TempDir(), []forTurnFixture{
		{file: "a.md", id: "p1", notify: format.NotifyPassive, created: "2026-01-01T00:00:00Z", body: "p1"},
		{file: "b.md", id: "n1", created: "2026-01-01T00:00:01Z", body: "n1"},
		{file: "c.md", id: "n2", created: "2026-01-01T00:00:02Z", body: "n2"},
		{file: "d.md", id: "u1", priority: format.PriorityUrgent, created: "2026-01-01T00:00:03Z", body: "u1"},
	})
	oldClaim := claimInboxNewToCur
	claimInboxNewToCur = func(root *fsq.DeliveryRoot, agent, filename string) error {
		for _, other := range []string{"a.md", "b.md", "c.md"} {
			if err := fsq.MoveNewToCur(root, agent, other); err != nil {
				return err
			}
		}
		return oldClaim(root, agent, filename)
	}
	t.Cleanup(func() { claimInboxNewToCur = oldClaim })

	out, err := captureStdout(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--for-turn", "--limit", "1", "--json"})
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	var got drainForTurnResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got.Count != 1 || got.Drained[0].ID != "u1" || got.Remaining != (forTurnRemaining{}) {
		t.Fatalf("result = %s", out)
	}
}
