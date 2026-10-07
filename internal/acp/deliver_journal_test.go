package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Bead agent-message-queue-z12 (Pro review of #965 r1): a cockpit prompt that
// reached the recipient inbox was reported as an error when the event journal
// write failed afterwards, so Buzz cancelled and re-prompted a delivered
// message, and a replay could deliver a second copy. The journal is now a
// claim taken before delivery, modeled on the mailbox steer claim: a failed
// claim write refuses before anything is delivered, so the inbox never holds
// a message the caller was told failed.
func TestFailedJournalClaimRefusesBeforeDelivery(t *testing.T) {
	root := canonicalTempDir(t)
	for _, handle := range []string{"buzz", "agent"} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Root: root, Me: "buzz", To: "agent"}

	// The journal directory is unwritable, so the claim cannot be created.
	journal := filepath.Join(root, "agents", "buzz", "outbox", "acp-events")
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(journal, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(journal, 0o700) })

	event := strings.Repeat("a", 64)
	if _, err := DeliverCockpitPrompt(cfg, "build the thing", cockpitThread("session/s1"), event); err == nil {
		t.Fatal("a prompt whose claim cannot be created delivered anyway; want a refusal")
	}
	if n := len(inboxPrompts(t, root)); n != 0 {
		t.Fatalf("inbox holds %d messages after a refused claim; want none delivered", n)
	}

	// A replay of the same event refuses the same way: the inbox never
	// gains a copy whose delivery was reported as failed.
	if _, err := DeliverCockpitPrompt(cfg, "build the thing", cockpitThread("session/s1"), event); err == nil {
		t.Fatal("replay with an unwritable journal delivered anyway; want a refusal")
	}
	if n := len(inboxPrompts(t, root)); n != 0 {
		t.Fatalf("inbox holds %d messages after refused replays; want none", n)
	}
}

// TestReplayDeliversWhenClaimExistsButInboxEmpty covers the failed first
// attempt: the claim was written, then the process died (or the inbox write
// failed) before the message reached the inbox. Because the claim records
// only identity and no outcome, the replay checks the inbox, finds the
// message absent, and delivers exactly one copy under the claimed id
// instead of reporting a delivery that never happened (review of #976).
func TestReplayDeliversWhenClaimExistsButInboxEmpty(t *testing.T) {
	root := canonicalTempDir(t)
	for _, handle := range []string{"buzz", "agent"} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Root: root, Me: "buzz", To: "agent"}

	event := strings.Repeat("b", 64)
	created := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	claim := eventRecord{
		Schema:    1,
		EventID:   event,
		MessageID: "2026-10-07T09-00-00.000Z_pid1_deadbeef",
		To:        "agent",
		Thread:    cockpitThread("session/s1"),
		Created:   created.Format(time.RFC3339Nano),
	}
	if err := rememberEvent(cfg, claim); err != nil {
		t.Fatal(err)
	}

	delivery, err := DeliverCockpitPrompt(cfg, "build the thing", cockpitThread("session/s1"), event)
	if err != nil {
		t.Fatalf("replay with an unmet claim refused: %v", err)
	}
	if delivery.Duplicate {
		t.Fatal("replay reported a duplicate for a message the inbox never held")
	}
	if delivery.MessageID != claim.MessageID {
		t.Fatalf("replay delivered %s; want the claimed id %s", delivery.MessageID, claim.MessageID)
	}
	ids := inboxPrompts(t, root)
	if len(ids) != 1 || ids[0] != claim.MessageID {
		t.Fatalf("inbox holds %v after the replay; want exactly [%s]", ids, claim.MessageID)
	}

	// A further replay now finds the message in the inbox and reports a
	// duplicate instead of writing a second copy.
	again, err := DeliverCockpitPrompt(cfg, "build the thing", cockpitThread("session/s1"), event)
	if err != nil {
		t.Fatalf("replay after a proven delivery failed: %v", err)
	}
	if !again.Duplicate || again.MessageID != claim.MessageID {
		t.Fatalf("replay after delivery = %+v; want a duplicate of the claimed id", again)
	}
	if n := len(inboxPrompts(t, root)); n != 1 {
		t.Fatalf("inbox holds %d messages after the duplicate replay; want one", n)
	}
}
