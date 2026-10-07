package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
