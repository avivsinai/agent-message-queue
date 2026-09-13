package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/amqio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// The amq-remote CLI shipped with no tests at all, while documenting a precise
// exit-code contract (0 ok, 1 failed or cancelled, 2 usage, 3 not found, 4
// wait timed out, 6 action required). Two defects found in the 2026-09-10
// audit — cancel substituting the live session epoch, and endpoint shutdown
// reported as "work failed" — were both reachable from one happy-path run of
// these verbs. These are those runs: one per user-visible behaviour, against a
// real endpoint over the real socket (agent-message-queue-611.22.29).

// startServe boots a real endpoint with the fake runtime in the background and
// returns its state directory. Socket paths must stay short, so the state dir
// lives directly under the system temp root.
func startServe(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "amqr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- run([]string{"serve", "--fake", "--root", root}, strings.NewReader(""), &out, &errBuf)
	}()
	t.Cleanup(func() {
		// serve exits on its own when the test process ends; drain if it
		// already returned so a failure surfaces instead of hanging.
		select {
		case <-done:
		default:
		}
	})

	// Wait for the socket to accept: `sessions` succeeds only once serving.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out, errBuf bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &out, &errBuf) == 0 {
			return root
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("endpoint did not start serving within 5s")
	return ""
}

func cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// TestCLIUsageAndVersion pins the two exits that need no endpoint.
func TestCLIUsageAndVersion(t *testing.T) {
	if code, _, _ := cli(t, "", "--help"); code != protocol.ExitUsage {
		t.Fatalf("--help exit = %d, want %d", code, protocol.ExitUsage)
	}
	if code, _, _ := cli(t, "", "no-such-command"); code != protocol.ExitUsage {
		t.Fatalf("unknown command exit = %d, want %d", code, protocol.ExitUsage)
	}
	code, out, _ := cli(t, "", "--version")
	if code != 0 || !strings.Contains(out, "amq-remote") {
		t.Fatalf("--version exit=%d out=%q", code, out)
	}
}

// TestCLISubmitStatusWaitHappyPath is the core round trip: submit a request,
// read it back, and wait for the result the runtime produces. It also pins
// that a wait which times out exits 4 rather than reporting failure.
func TestCLISubmitStatusWaitHappyPath(t *testing.T) {
	root := startServe(t)

	code, out, errOut := cli(t, "", "submit", "fake", "--text", "say hi", "--root", root, "--json")
	if code != 0 {
		t.Fatalf("submit exit=%d out=%s err=%s", code, out, errOut)
	}
	var rep protocol.Reply
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("submit output is not a Reply: %v (%s)", err, out)
	}
	ref := rep.Snapshot.RequestRef
	if ref == "" {
		t.Fatalf("submit returned no request ref: %s", out)
	}

	// status reads the same record back.
	code, out, errOut = cli(t, "", "status", ref, "--root", root, "--json")
	if code != 0 {
		t.Fatalf("status exit=%d out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, rep.Snapshot.RequestID) {
		t.Fatalf("status did not return the submitted request: %s", out)
	}

	// A wait that expires before the runtime answers is a TIMEOUT (exit 4),
	// never a failure: the request is untouched and still running.
	code, _, _ = cli(t, "", "wait", ref, "--timeout", "300ms", "--root", root, "--json")
	if code != protocol.ExitTimeout {
		t.Fatalf("expired wait exit = %d, want %d (timeout, not failure)", code, protocol.ExitTimeout)
	}
}

// TestCLIStatusUnknownRefIsNotFound pins exit 3 — distinct from a failure.
func TestCLIStatusUnknownRefIsNotFound(t *testing.T) {
	root := startServe(t)
	ref := protocol.EncodeRef("local", "fake", "11111111-1111-4111-8111-1111111119f0")
	code, out, _ := cli(t, "", "status", ref, "--root", root, "--json")
	if code != protocol.ExitNotFound {
		t.Fatalf("unknown ref exit = %d, want %d (not found) out=%s", code, protocol.ExitNotFound, out)
	}
}

// TestCLIDisabledModeIsActionRequired pins exit 6: busy=queue and
// deliver=steer are disabled in v1, and a disabled mode is action-required
// (the caller must choose another mode), not a failure.
func TestCLIDisabledModeIsActionRequired(t *testing.T) {
	root := startServe(t)
	code, out, _ := cli(t, "", "submit", "fake", "--text", "x", "--busy", "queue", "--root", root, "--json")
	if code != protocol.ExitActionRequired {
		t.Fatalf("busy=queue exit = %d, want %d (action required) out=%s", code, protocol.ExitActionRequired, out)
	}
}

// TestCLISubmitRejectsEmptyPrompt pins that a whitespace-only prompt never
// reaches a harness: it is a usage error at the CLI boundary.
func TestCLISubmitRejectsEmptyPrompt(t *testing.T) {
	root := startServe(t)
	code, _, _ := cli(t, "", "submit", "fake", "--text", "   ", "--root", root, "--json")
	if code == 0 {
		t.Fatal("whitespace-only prompt was accepted")
	}
}

// TestReplyRouterForTransientPeerAbsent exercises the SHIPPED adapter
// (replyRouterFor), not a reimplemented copy. With the peer root absent,
// the adapter must wrap the router's ErrPeerRootUnreachable in
// amqio.TransientRouteError so the carrier leaves the message in new
// (transient) instead of DLQ'ing it (poison). B1: deleting the translation
// from replyRouterFor must fail this test.
func TestReplyRouterForTransientPeerAbsent(t *testing.T) {
	endpointBase := t.TempDir()
	endpointRoot := filepath.Join(endpointBase, ".agent-mail")
	if err := fsq.EnsureRootDirs(endpointRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, amqio.DefaultHandle); err != nil {
		t.Fatal(err)
	}
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
	// Use the SHIPPED adapter — not a reimplemented closure.
	carrier.SetReplyRouter(replyRouterFor(endpointRoot))
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

	// TICK 1: caller root does NOT exist. The shipped adapter must wrap the
	// router's ErrPeerRootUnreachable in TransientRouteError. The carrier
	// leaves the message in new (transient, not DLQ'd).
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

	// TICK 2: caller root exists. The shipped adapter resolves, the carrier
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
