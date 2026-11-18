package followup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func openTestRoot(t *testing.T) (string, *fsq.DeliveryRoot) {
	t.Helper()
	base := t.TempDir()
	if err := fsq.EnsureRootDirs(base); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(base, agent); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return base, root
}

func armTestTimer(t *testing.T, root *fsq.DeliveryRoot, sent time.Time) Timer {
	t.Helper()
	requestID, err := format.NewMessageID(sent)
	if err != nil {
		t.Fatal(err)
	}
	timer, err := New(requestID, "bob", "p2p/alice__bob", "Review the parser", format.KindTodo, sent, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(root, "alice", timer); err != nil {
		t.Fatal(err)
	}
	return timer
}

func inboxNames(t *testing.T, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(fsq.AgentInboxNew(base, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestPromoteDeliversOneNoticeWhenDueAndUnanswered(t *testing.T) {
	base, root := openTestRoot(t)
	sent := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	timer := armTestTimer(t, root, sent)

	if err := Promote(root, "alice", timer.Due.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if names := inboxNames(t, base); len(names) != 0 {
		t.Fatalf("before due the inbox holds %v, want nothing", names)
	}

	if err := Promote(root, "alice", timer.Due); err != nil {
		t.Fatal(err)
	}
	names := inboxNames(t, base)
	if !slices.Equal(names, []string{timer.NoticeID + ".md"}) {
		t.Fatalf("at due the inbox holds %v, want the one notice %s.md", names, timer.NoticeID)
	}
	h, err := format.ReadHeaderFile(filepath.Join(fsq.AgentInboxNew(base, "alice"), names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if h.From != "alice" || !slices.Equal(h.To, []string{"alice"}) || h.Thread != "p2p/alice__bob" ||
		!slices.Equal(h.Refs, []string{timer.RequestID}) || h.Kind != format.KindStatus ||
		h.Priority != format.PriorityNormal || !slices.Equal(h.Labels, []string{Label}) ||
		h.Subject != "Still open with bob: Review the parser" || h.Created != timer.Due.Format(time.RFC3339Nano) {
		t.Fatalf("notice header = %+v", h)
	}
	if timers, err := List(root, "alice"); err != nil || len(timers) != 0 {
		t.Fatalf("timers after the notice = %v (err %v), want none", timers, err)
	}

	if err := Promote(root, "alice", timer.Due.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if names := inboxNames(t, base); len(names) != 1 {
		t.Fatalf("a second pass left %v, want the one notice", names)
	}
}

// In the 2026-11-18 docs audit (C3), a worker's ETA silenced the reminder for an open todo.
func TestPromoteStillOpenTodoAfterProgressReply(t *testing.T) {
	base, root := openTestRoot(t)
	timer := armTestTimer(t, root, time.Now())
	progress := testMessage(t, timer, "bob", "alice", format.KindStatus, nil)
	progress.Body = "ETA ten minutes"
	if _, err := fsq.DeliverToInbox(root, "alice", progress.Header.ID+".md", marshal(t, progress)); err != nil {
		t.Fatal(err)
	}
	if err := Promote(root, "alice", timer.Due); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(inboxNames(t, base), timer.NoticeID+".md") {
		t.Fatal("open todo received no follow-up notice after a progress reply")
	}
}

func TestPromoteDropsTimerWhenRequestSettled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle func(t *testing.T, root *fsq.DeliveryRoot, timer Timer)
	}{
		{"recipient closed the todo", func(t *testing.T, root *fsq.DeliveryRoot, timer Timer) {
			reply := testMessage(t, timer, "bob", "alice", format.KindStatus, []string{"done"})
			if _, err := fsq.DeliverToInbox(root, "alice", reply.Header.ID+".md", marshal(t, reply)); err != nil {
				t.Fatal(err)
			}
		}},
		{"requester withdrew the request", func(t *testing.T, root *fsq.DeliveryRoot, timer Timer) {
			withdrawn := testMessage(t, timer, "alice", "bob", format.KindDecision, []string{"withdrawn"})
			if _, err := root.WriteFileAtomic(fsq.MailboxRootRelativePath("alice", fsq.MailboxOutboxSent), withdrawn.Header.ID+".md", marshal(t, withdrawn), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, root := openTestRoot(t)
			sent := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
			timer := armTestTimer(t, root, sent)
			tc.settle(t, root, timer)
			before := inboxNames(t, base)

			if err := Promote(root, "alice", timer.Due); err != nil {
				t.Fatal(err)
			}
			if names := inboxNames(t, base); !slices.Equal(names, before) {
				t.Fatalf("inbox after promotion = %v, want %v and no notice", names, before)
			}
			if timers, err := List(root, "alice"); err != nil || len(timers) != 0 {
				t.Fatalf("timers after a settled request = %v (err %v), want none", timers, err)
			}
		})
	}
}

func testMessage(t *testing.T, timer Timer, from, to, kind string, labels []string) format.Message {
	t.Helper()
	at := timer.Sent.Add(time.Minute)
	id, err := format.NewMessageID(at)
	if err != nil {
		t.Fatal(err)
	}
	return format.Message{Header: format.Header{
		Schema: 1, ID: id, From: from, To: []string{to}, Thread: timer.Thread,
		Created: at.Format(time.RFC3339Nano), Refs: []string{timer.RequestID}, Kind: kind, Labels: labels,
	}, Body: "ok"}
}

func marshal(t *testing.T, msg format.Message) []byte {
	t.Helper()
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return data
}
