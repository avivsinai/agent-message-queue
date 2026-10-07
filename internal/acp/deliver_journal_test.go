package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
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

// Review of #976 r2 P1: a claim that exists but was never fsynced (the
// winner's exclusive create succeeded, its journal-dir sync failed, and the
// attempt refused) must not let a replay publish until durability holds:
// the adopt path re-syncs the journal directory chain under the post lock,
// and a replay with a still-failing sync refuses BEFORE writing the inbox
// message. Only a replay whose claim sync succeeds publishes, so a crash
// can never keep the message and lose the claim (the seed of a second copy
// under a fresh id).
func TestReplayWithFailingJournalSyncRefusesBeforePublishing(t *testing.T) {
	root := canonicalTempDir(t)
	for _, handle := range []string{"buzz", "agent"} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Root: root, Me: "buzz", To: "agent"}
	event := strings.Repeat("e", 64)
	thread := cockpitThread("session/s1")

	// A claim already on disk from a crashed first attempt.
	claim := eventRecord{
		Schema:    1,
		EventID:   event,
		MessageID: "2026-10-07T09-30-00.000Z_pid1_cafe0000",
		To:        "agent",
		Thread:    thread,
		Created:   time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	}
	if err := rememberEvent(cfg, claim); err != nil {
		t.Fatal(err)
	}

	// Every journal-directory sync fails: the claim stays non-durable.
	// Package-wide fault so it reaches the root the deliver path opens.
	fsq.SetPackageSyncDirFaultForTest(func(dir string) error {
		return fmt.Errorf("injected sync failure")
	})
	t.Cleanup(func() { fsq.SetPackageSyncDirFaultForTest(nil) })

	if _, err := DeliverCockpitPrompt(cfg, "build the thing", thread, event); err == nil {
		t.Fatal("replay with an unsyncable claim delivered anyway; want a refusal")
	}
	if n := len(inboxPrompts(t, root)); n != 0 {
		t.Fatalf("inbox holds %d messages while the claim is non-durable; want none", n)
	}

	// The sync heals: clear the fault; the replay now delivers under the
	// claimed id, and the inbox never held a copy whose delivery was
	// refused.
	fsq.SetPackageSyncDirFaultForTest(nil)
	delivery, err := DeliverCockpitPrompt(cfg, "build the thing", thread, event)
	if err != nil {
		t.Fatalf("replay after the sync healed: %v", err)
	}
	if delivery.Duplicate || delivery.MessageID != claim.MessageID {
		t.Fatalf("replay = %+v; want a first delivery of %s", delivery, claim.MessageID)
	}
	ids := inboxPrompts(t, root)
	if len(ids) != 1 || ids[0] != claim.MessageID {
		t.Fatalf("inbox holds %v; want exactly [%s]", ids, claim.MessageID)
	}
}

// Review of #976 r2 P2: the inbox proof reads both inbox locations through
// the pinned root and requires a readable regular message whose header id
// matches the claim. A directory or a dangling symlink at <id>.md is not a
// delivery; the attempt refuses instead of reporting a proven duplicate.
func TestInboxProofRefusesNonMessageEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{"directory", func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling symlink", func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join("..", "..", "gone", "target"), path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := canonicalTempDir(t)
			for _, handle := range []string{"buzz", "agent"} {
				if err := fsq.EnsureAgentDirs(root, handle); err != nil {
					t.Fatal(err)
				}
			}
			cfg := Config{Root: root, Me: "buzz", To: "agent"}
			event := strings.Repeat("f", 64)
			thread := cockpitThread("session/s1")

			claim := eventRecord{
				Schema:    1,
				EventID:   event,
				MessageID: "2026-10-07T09-45-00.000Z_pid1_face0000",
				To:        "agent",
				Thread:    thread,
				Created:   time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
			}
			if err := rememberEvent(cfg, claim); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, filepath.Join(fsq.AgentInboxNew(root, "agent"), claim.MessageID+".md"))

			if _, err := DeliverCockpitPrompt(cfg, "build the thing", thread, event); err == nil {
				t.Fatalf("a %s at the claimed inbox name reported delivery; want a refusal", tc.name)
			}
		})
	}
}

// Review of #976 P1 b: the inbox absence check and the write are one
// critical section under the event's post lock. When the first copy was
// drained to cur/ before the replay ran, the replay still finds it (in
// cur) and reports a duplicate instead of writing a second copy into new/.
func TestReplayFindsACopyDrainedToCur(t *testing.T) {
	root := canonicalTempDir(t)
	for _, handle := range []string{"buzz", "agent"} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Root: root, Me: "buzz", To: "agent"}
	event := strings.Repeat("c", 64)
	thread := cockpitThread("session/s1")

	first, err := DeliverCockpitPrompt(cfg, "build the thing", thread, event)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// The consumer drains new/ to cur/, the normal maildir path.
	if err := os.Rename(
		filepath.Join(fsq.AgentInboxNew(root, "agent"), first.MessageID+".md"),
		filepath.Join(fsq.AgentInboxCur(root, "agent"), first.MessageID+".md"),
	); err != nil {
		t.Fatal(err)
	}

	again, err := DeliverCockpitPrompt(cfg, "build the thing", thread, event)
	if err != nil {
		t.Fatalf("replay after drain: %v", err)
	}
	if !again.Duplicate || again.MessageID != first.MessageID {
		t.Fatalf("replay after drain = %+v; want a duplicate of %s", again, first.MessageID)
	}
	ids := inboxPrompts(t, root)
	if n := len(ids); n != 1 || ids[0] != first.MessageID {
		t.Fatalf("inbox holds %v after the drained replay; want exactly [%s]", ids, first.MessageID)
	}
}

// Review of #976 P2 d: schema-1 journals written by the current release
// have no created field. A replay proves delivery from the inbox by the
// message id, takes created from the delivered message's header, and
// reports a duplicate — it must not refuse.
func TestLegacyJournalWithoutCreatedIsProvenFromTheInbox(t *testing.T) {
	root := canonicalTempDir(t)
	for _, handle := range []string{"buzz", "agent"} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Root: root, Me: "buzz", To: "agent"}
	event := strings.Repeat("d", 64)
	thread := cockpitThread("session/s1")

	// The exact pre-upgrade journal bytes: identity only, no created.
	created := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	messageID, err := format.NewMessageID(created)
	if err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf(
		`{"schema":1,"event_id":%q,"message_id":%q,"to":%q,"thread":%q}`+"\n",
		event, messageID, "agent", thread,
	)
	journalDir := filepath.Join(root, "agents", "buzz", "outbox", "acp-events")
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journalDir, event+".json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	// The delivered message exists in cur/ (drained long ago).
	message := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      messageID,
			From:    "buzz",
			To:      []string{"agent"},
			Thread:  thread,
			Created: created.Format(time.RFC3339Nano),
			Labels:  []string{"acp", "cockpit", "nostr:" + event},
		},
		Body: "build the thing",
	}
	raw, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxCur(root, "agent"), messageID+".md"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	delivery, err := DeliverCockpitPrompt(cfg, "build the thing", thread, event)
	if err != nil {
		t.Fatalf("replay of a legacy journal refused: %v", err)
	}
	if !delivery.Duplicate || delivery.MessageID != messageID {
		t.Fatalf("replay of a legacy journal = %+v; want a duplicate of %s", delivery, messageID)
	}
	if !delivery.Created.Equal(created) {
		t.Fatalf("replay created = %v; want the delivered message's header time %v", delivery.Created, created)
	}
	if n := len(inboxPrompts(t, root)); n != 1 {
		t.Fatalf("inbox holds %d messages after the legacy replay; want one", n)
	}
}
