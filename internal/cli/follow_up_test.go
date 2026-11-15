package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/followup"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// armFollowUpForTest writes a follow-up timer for a request from me to bob,
// sent at sent and due after the given duration.
func armFollowUpForTest(t *testing.T, root, me, to string, sent time.Time, after time.Duration) followup.Timer {
	t.Helper()
	requestID, err := format.NewMessageID(sent)
	if err != nil {
		t.Fatal(err)
	}
	timer, err := followup.New(requestID, to, canonicalP2P(me, to), "Review the parser", format.KindTodo, sent, after)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deliveryRoot.Close() }()
	if err := followup.Write(deliveryRoot, me, timer); err != nil {
		t.Fatal(err)
	}
	return timer
}

func initFollowUpRootForTest(t *testing.T) string {
	t.Helper()
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "alice,bob"}); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSendFollowUpArmsTimerAndReportsDue(t *testing.T) {
	root := initFollowUpRootForTest(t)
	before := time.Now().Truncate(time.Second)
	out, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "alice", "--to", "bob", "--kind", "todo", "--follow-up", "30m", "--body", "review the parser", "--json"})
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var result struct {
		ID       string `json:"id"`
		FollowUp struct {
			Due      string `json:"due"`
			After    string `json:"after"`
			NoticeID string `json:"notice_id"`
			Error    string `json:"error"`
		} `json:"follow_up"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	due, err := time.Parse(time.RFC3339, result.FollowUp.Due)
	if err != nil || due.Before(before.Add(30*time.Minute)) || due.After(time.Now().Add(30*time.Minute)) ||
		result.FollowUp.After != "30m0s" || result.FollowUp.NoticeID == "" || result.FollowUp.Error != "" {
		t.Fatalf("follow_up = %+v (due parse err %v), want due 30m after the send", result.FollowUp, err)
	}

	data, err := os.ReadFile(filepath.Join(fsq.AgentFollowUps(root, "alice"), result.ID+".json"))
	if err != nil {
		t.Fatalf("timer file: %v", err)
	}
	var timer followup.Timer
	if err := json.Unmarshal(data, &timer); err != nil {
		t.Fatal(err)
	}
	if timer.RequestID != result.ID || timer.To != "bob" || timer.NoticeID != result.FollowUp.NoticeID ||
		timer.Due.Format(time.RFC3339) != result.FollowUp.Due || timer.RequestKind != format.KindTodo {
		t.Fatalf("timer = %+v, want the request %s to bob with the reported due and notice", timer, result.ID)
	}
}

func TestDrainShowsDueFollowUpNotice(t *testing.T) {
	root := initFollowUpRootForTest(t)
	timer := armFollowUpForTest(t, root, "alice", "bob", time.Now().Add(-time.Hour), 30*time.Minute)

	result := runDrainJSON(t, root, "alice", 0, false)
	if result.Count != 1 || result.Drained[0].ID != timer.NoticeID || result.Drained[0].Subject != "No reply from bob: Review the parser" {
		t.Fatalf("drained = %+v, want the one follow-up notice %s", result.Drained, timer.NoticeID)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentFollowUps(root, "alice"), timer.RequestID+".json")); !os.IsNotExist(err) {
		t.Fatalf("timer after its notice was drained: %v, want it gone", err)
	}
}

func TestDoctorOpsReportsPendingFollowUps(t *testing.T) {
	root := initFollowUpRootForTest(t)
	sent := time.Now()
	soon := armFollowUpForTest(t, root, "alice", "bob", sent, 12*time.Minute)
	armFollowUpForTest(t, root, "alice", "bob", sent, time.Hour)

	result := runOpsChecks(root, "test_source", false)
	var alice *opsAgent
	for i := range result.Agents {
		if result.Agents[i].Handle == "alice" {
			alice = &result.Agents[i]
		}
	}
	if alice == nil {
		t.Fatal("no alice row")
	}
	if alice.FollowUpsPending != 2 || alice.FollowUpsOverdue != 0 || alice.NextFollowUpDue != soon.Due.Format(time.RFC3339) {
		t.Fatalf("alice follow-ups = %d pending, %d overdue, next %q; want 2, 0, %s",
			alice.FollowUpsPending, alice.FollowUpsOverdue, alice.NextFollowUpDue, soon.Due.Format(time.RFC3339))
	}
	due, _ := time.Parse(time.RFC3339, alice.NextFollowUpDue)
	if note := followUpRowNote(*alice, due.Add(-12*time.Minute)); note != ", 2 follow-ups (next due in 12m)" {
		t.Fatalf("text row note = %q", note)
	}
}
