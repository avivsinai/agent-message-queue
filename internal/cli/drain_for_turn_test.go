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

// forTurnMailbox delivers the fixtures to alice from bob. File names and
// creation times run against the turn order, so only the tiers order them.
func forTurnMailbox(t *testing.T, fixtures []forTurnFixture) string {
	t.Helper()
	root := t.TempDir()
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
// in full, then each passive message as a digest, keeps the originals in
// inbox/cur, and writes the drained receipt for every message it claimed.
func TestDrainForTurnDigestsPassiveMailAfterActionableMail(t *testing.T) {
	long := strings.Repeat("é", 100) + "\n\n" + strings.Repeat("ü", 100)
	root := forTurnMailbox(t, []forTurnFixture{
		{file: "a.md", id: "passive", priority: format.PriorityNormal, notify: format.NotifyPassive, created: "2026-01-01T00:00:00Z", body: long, refs: []string{"r1", "r2"}, labels: []string{"fyi"}},
		{file: "b.md", id: "normal", priority: format.PriorityNormal, created: "2026-01-01T00:00:01Z", body: "normal body"},
		{file: "c.md", id: "urgent", priority: format.PriorityUrgent, created: "2026-01-01T00:00:02Z", body: "urgent body"},
	})

	out, err := captureStdout(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--for-turn", "--json"})
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
	if got.Count != 3 || got.Remaining != (forTurnRemaining{}) || len(got.Passive) != 1 {
		t.Fatalf("result = %+v", got)
	}
	d := got.Passive[0]
	want := strings.Repeat("é", 100) + " " + strings.Repeat("ü", 58) + "…"
	if d.ID != "passive" || d.From != "bob" || d.Thread != "p2p/alice__bob" || d.Kind != format.KindStatus ||
		d.Priority != format.PriorityNormal || d.Subject != "subject passive" ||
		!slices.Equal(d.Refs, []string{"r1", "r2"}) || !slices.Equal(d.Labels, []string{"fyi"}) ||
		d.Preview != want || utf8.RuneCountInString(d.Preview) != 160 || !d.Truncated ||
		d.Read != "amq read --me alice --id passive" {
		t.Fatalf("digest = %+v", d)
	}
	if strings.Count(out, "ü") != 58 {
		t.Fatalf("output carries a passive body: %s", out)
	}
	original, err := os.ReadFile(filepath.Join(fsq.AgentInboxCur(root, "alice"), "a.md"))
	if err != nil || !strings.Contains(string(original), long) {
		t.Fatalf("original in cur: %q, %v", original, err)
	}
	for _, id := range []string{"urgent", "normal", "passive"} {
		receipts, err := receipt.List(root, "alice", receipt.ListFilter{MsgID: id, Stage: receipt.StageDrained})
		if err != nil || len(receipts) != 1 {
			t.Fatalf("drained receipts for %s = %+v, %v", id, receipts, err)
		}
	}
}

// The turn order is applied before --limit: with a limit of two, the batch
// is the urgent message and the older normal one, and the rest stay unread.
func TestDrainForTurnOrdersBeforeTheLimit(t *testing.T) {
	root := forTurnMailbox(t, []forTurnFixture{
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

// Without --for-turn, drain prints what it printed before the flag existed,
// in text with bodies and in JSON, for the same mixed mailbox.
func TestDrainWithoutForTurnIsUnchanged(t *testing.T) {
	fixtures := []forTurnFixture{
		{file: "a.md", id: "passive", priority: format.PriorityNormal, notify: format.NotifyPassive, created: "2026-01-01T00:00:00Z", body: "fyi", refs: []string{"r1"}, labels: []string{"fyi"}},
		{file: "b.md", id: "normal", created: "2026-01-01T00:00:01Z", body: "normal body"},
		{file: "c.md", id: "urgent", priority: format.PriorityUrgent, created: "2026-01-01T00:00:02Z", body: "urgent body"},
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--include-body"}, wantPlainDrainText},
		{[]string{"--json"}, wantPlainDrainJSON},
	} {
		root := forTurnMailbox(t, fixtures)
		out, err := captureStdout(t, func() error {
			return runDrain(append([]string{"--root", root, "--me", "alice"}, tc.args...))
		})
		if err != nil || out != tc.want {
			t.Fatalf("drain %v: error %v, output:\n%s", tc.args, err, out)
		}
	}
}

// Captured from the drain that shipped before --for-turn.
const wantPlainDrainText = `[AMQ] 3 new message(s) for alice:

- From: bob
  Thread: p2p/alice__bob
  ID: passive
  Subject: subject passive
  Priority: normal
  Kind: status
  Created: 2026-01-01T00:00:00Z
  Body:
fyi

---
- From: bob
  Thread: p2p/alice__bob
  ID: normal
  Subject: subject normal
  Priority: -
  Kind: status
  Created: 2026-01-01T00:00:01Z
  Body:
normal body

---
- From: bob
  Thread: p2p/alice__bob
  ID: urgent
  Subject: subject urgent
  Priority: urgent
  Kind: status
  Created: 2026-01-01T00:00:02Z
  Body:
urgent body

---
`

const wantPlainDrainJSON = `{
  "drained": [
    {
      "id": "passive",
      "from": "bob",
      "to": [
        "alice"
      ],
      "thread": "p2p/alice__bob",
      "subject": "subject passive",
      "created": "2026-01-01T00:00:00Z",
      "notify": "passive",
      "priority": "normal",
      "kind": "status",
      "labels": [
        "fyi"
      ],
      "moved_to_cur": true
    },
    {
      "id": "normal",
      "from": "bob",
      "to": [
        "alice"
      ],
      "thread": "p2p/alice__bob",
      "subject": "subject normal",
      "created": "2026-01-01T00:00:01Z",
      "kind": "status",
      "moved_to_cur": true
    },
    {
      "id": "urgent",
      "from": "bob",
      "to": [
        "alice"
      ],
      "thread": "p2p/alice__bob",
      "subject": "subject urgent",
      "created": "2026-01-01T00:00:02Z",
      "priority": "urgent",
      "kind": "status",
      "moved_to_cur": true
    }
  ],
  "count": 3
}
`
