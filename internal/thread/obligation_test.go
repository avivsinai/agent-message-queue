package thread

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Review of the news rule: a later report from the owner that reached the
// requester's handle in another session counted as news for this requester.
func TestNewsRequiresDeliveryToOriginalRequester(t *testing.T) {
	now := time.Now()
	ask := Entry{ID: "ask", Kind: "todo", From: "lead", To: []string{"worker"}, Thread: "task", NewsBy: now.Format(time.RFC3339), RawTime: now,
		Occurrences: []Occurrence{{Agent: "lead", Session: "s1", Dir: fsq.MailboxOutboxSent}, {Agent: "worker", Session: "s1", Dir: fsq.MailboxInboxCur}}}
	report := Entry{ID: "report", Kind: "status", From: "worker", To: []string{"lead"}, Thread: "other", RawTime: now.Add(time.Second), ReplyTo: "worker@s1",
		Occurrences: []Occurrence{{Agent: "worker", Session: "s1", Dir: fsq.MailboxOutboxSent}, {Agent: "lead", Session: "s2", Dir: fsq.MailboxInboxCur}}}
	rows := ReduceObligations(nil, []Entry{ask, report}, time.Time{}, nil).Obligations
	if len(rows) != 1 || rows[0].OwnerReplied {
		t.Fatalf("delivery to lead@s2 counted as news to lead@s1: %+v", rows)
	}
	report.Occurrences[1].Session = "s1"
	report.ReplyTo = ""
	rows = ReduceObligations(nil, []Entry{ask, report}, time.Time{}, nil).Obligations
	if len(rows) != 1 || !rows[0].OwnerReplied {
		t.Fatalf("a later report to the requester itself is not news: %+v", rows)
	}
}
