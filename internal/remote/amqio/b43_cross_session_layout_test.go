package amqio

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression for agent-message-queue-611.22.43: the cross-session arm of
// destination() (project == "" && reply_to carries a session component) opened
// the routed peer root and returned it WITHOUT calling
// ValidateExistingMailboxLayout — the very gate the cross-project arm below it
// applies (D4). A routed session root whose mailbox did not exist was silently
// written into, creating a black-hole mailbox in someone else's root: the hole
// D4 closed one directory over. The fix applies the same gate to both arms.
//
// This test routes a cross-session reply to a peer root whose "codex" mailbox
// has NOT been provisioned and asserts:
//  1. The reply is refused (TransientRouteError), not silently delivered.
//  2. Nothing is written into the peer root (no black-hole mailbox created).
//  3. The command stays in new (answerable later when the mailbox exists).
func TestB43CrossSessionReplyToMissingMailboxIsRefusedNotCreated(t *testing.T) {
	endpointRoot := t.TempDir()
	callerRoot := t.TempDir() // a real root, but the codex mailbox is NOT provisioned
	for _, root := range []string{endpointRoot, callerRoot} {
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, DefaultHandle); err != nil {
		t.Fatal(err)
	}
	// Deliberately do NOT EnsureAgentDirs(callerRoot, "codex") — the mailbox
	// must not exist, which is exactly the hole this bead closes.

	store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
		return carrier.Publish(s, origin)
	}})
	carrier, err = New(endpointRoot, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	// Cross-session arm: reply_project empty, reply_to carries a session
	// component ("codex@session1") → crossRoot(origin) is true → router is
	// called with ("", "codex@session1"). Return a real root whose "codex"
	// mailbox does not exist.
	var routedProject, routedReplyTo string
	carrier.SetReplyRouter(func(replyProject, replyTo string) (string, string, error) {
		routedProject, routedReplyTo = replyProject, replyTo
		return callerRoot, "codex", nil
	})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111431","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"cross-session"}}`
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
		// reply_project empty + reply_to has "@" → cross-session arm (B7).
		ReplyTo: "codex@session1",
	}, Body: body}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(endpointRoot)
	if err != nil {
		t.Fatal(err)
	}
	droot, err := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()

	// ImportOnce must NOT silently create the mailbox in callerRoot. The
	// refusal is transient (the mailbox may be provisioned later), so the
	// carrier leaves the command in new and returns no error — the same
	// contract the cross-project arm has. We assert the *effect* (no
	// black-hole, command stays in new), not an error return.
	n, err := carrier.ImportOnce()
	if err != nil {
		t.Fatalf("ImportOnce errored for a transient cross-session refusal (611.22.43 — transient must be swallowed, command stays in new): %v", err)
	}
	if n != 0 {
		t.Fatalf("ImportOnce consumed %d message(s) for a cross-session reply to a missing mailbox (611.22.43 — transient refusal must leave the command in new)", n)
	}

	// The router must have been driven on the cross-session arm.
	if routedProject != "" || routedReplyTo != "codex@session1" {
		t.Fatalf("router called with (%q, %q), want (\"\", \"codex@session1\") for the cross-session arm (611.22.43)", routedProject, routedReplyTo)
	}

	// Nothing may have been written into the peer root: the codex mailbox
	// must STILL not exist there (no black-hole creation).
	if _, err := os.Stat(fsq.AgentInboxNew(callerRoot, "codex")); err == nil {
		entries, _ := os.ReadDir(fsq.AgentInboxNew(callerRoot, "codex"))
		t.Fatalf("codex mailbox created in peer root %s (611.22.43 — the gate must prevent black-hole creation): %d entries", callerRoot, len(entries))
	}

	// The command stays in new (answerable later), per D1.
	newPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "new", id+".md")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("command left new after cross-session route refusal (611.22.43 — must stay in new, answerable later): %v", err)
	}
}
