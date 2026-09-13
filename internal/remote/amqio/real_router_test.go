package amqio_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/amqio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestImportCrossProjectRealRouterTransientPeerAbsent reproduces B1: with the
// REAL router (cli.ResolveReplyRoute), an absent peer root is TRANSIENT — the
// message stays in new and is delivered on the next tick once the peer root is
// created. The old code permanently DLQ'd it because the router's error was
// always classified as poison.
//
// This test uses the REAL cli.ResolveReplyRoute, not a fake router, because
// the review proved that fake routers reach states production cannot. The
// fake router returns a path for a non-existent root — a state the production
// router never produces (it fails at resolveSessionRoot, not after).
func TestImportCrossProjectRealRouterTransientPeerAbsent(t *testing.T) {
	endpointBase := t.TempDir()
	endpointRoot := filepath.Join(endpointBase, ".agent-mail")
	if err := fsq.EnsureRootDirs(endpointRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, amqio.DefaultHandle); err != nil {
		t.Fatal(err)
	}
	// caller root lives in a sibling directory
	callerRoot := filepath.Join("..", "caller", ".agent-mail")
	amqrcData, _ := json.Marshal(map[string]any{
		"project": "endpoint",
		"root":    ".agent-mail",
		"peers":   map[string]string{"caller": callerRoot},
	})
	if err := os.WriteFile(filepath.Join(endpointBase, ".amqrc"), amqrcData, 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(endpointBase); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *amqio.Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
		return carrier.Publish(s, origin)
	}})
	carrier, err = amqio.New(endpointRoot, amqio.DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	carrier.SetReplyRouter(func(replyProject, replyTo string) (string, string, error) {
		return cli.ResolveReplyRoute(endpointRoot, replyProject, replyTo)
	})
	ep.Register(fake.New("fake", "e_1"))
	t.Cleanup(func() { _ = ep.Close() })

	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	defer func() { _ = droot.Close() }()

	now := time.Now()
	id, _ := format.NewMessageID(now)
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111381","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{amqio.DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
		FromProject: "caller", ReplyTo: "codex@collab", ReplyProject: "caller",
	}, Body: body}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInboxes(droot, []string{amqio.DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// TICK 1: caller root does NOT exist. The real router returns
	// ErrPeerRootUnreachable, which ResolveReplyRoute wraps in
	// TransientRouteError. The carrier leaves the message in new.
	n, _ := carrier.ImportOnce()
	if n != 0 {
		t.Fatalf("TICK1: message should stay in new (transient), but ImportOnce handled %d", n)
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(endpointRoot, amqio.DefaultHandle)); len(entries) != 1 {
		t.Fatalf("TICK1: message should be in new (transient), found %d", len(entries))
	}
	dlqDir := filepath.Join(endpointRoot, "agents", amqio.DefaultHandle, "dlq", "new")
	if entries, _ := os.ReadDir(dlqDir); len(entries) != 0 {
		t.Fatalf("TICK1: transient message was DLQ'd (B1 — absent peer root is not poison): %d", len(entries))
	}

	// Create the caller root, session, and codex mailbox between tick 1 and 2.
	callerBase := filepath.Dir(callerRoot)
	if err := os.MkdirAll(callerBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureRootDirs(callerRoot); err != nil {
		t.Fatal(err)
	}
	// Create the "collab" session root (replyTo was "codex@collab").
	sessionRoot := filepath.Join(callerRoot, "collab")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureRootDirs(sessionRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(sessionRoot, "codex"); err != nil {
		t.Fatal(err)
	}

	// TICK 2: caller root exists. The real router resolves, the carrier
	// delivers the reply, and the command is claimed.
	n, err = carrier.ImportOnce()
	if err != nil || n != 1 {
		t.Fatalf("TICK2: expected delivery after peer root created, got n=%d err=%v", n, err)
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxCur(endpointRoot, amqio.DefaultHandle)); len(entries) != 1 {
		t.Fatalf("TICK2: command not claimed into cur: %d", len(entries))
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(sessionRoot, "codex")); len(entries) == 0 {
		t.Fatal("TICK2: no reply delivered to caller's codex inbox")
	}
}
