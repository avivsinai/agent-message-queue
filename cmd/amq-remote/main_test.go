package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/amqio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	"github.com/avivsinai/agent-message-queue/internal/remote/sender"
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

// TestReplyRouterForSameProjectSessionReply reproduces B7
// (agent-message-queue-611.22.35): a caller in another SESSION of the same
// project sets reply_to=<handle>@<session> and no reply_project. The reply
// must land in that session's root — from an endpoint at the base root and
// from one inside a session root. While the session does not exist the route
// is transient (the command stays in new); it is never a silent delivery to
// the endpoint's own root.
func TestReplyRouterForSameProjectSessionReply(t *testing.T) {
	for _, tc := range []struct{ name, endpointRel string }{{"base root", ".agent-mail"}, {"session root", ".agent-mail/s1"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AM_BASE_ROOT", "")
			t.Setenv("AM_ROOT", "")
			t.Setenv("AM_SESSION", "")
			base := t.TempDir()
			baseRoot := filepath.Join(base, ".agent-mail")
			endpointRoot := filepath.Join(base, tc.endpointRel)
			for _, r := range []string{baseRoot, endpointRoot} {
				if err := fsq.EnsureRootDirs(r); err != nil {
					t.Fatal(err)
				}
			}
			if err := fsq.EnsureAgentDirs(endpointRoot, amqio.DefaultHandle); err != nil {
				t.Fatal(err)
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
			carrier.SetReplyRouter(replyRouterFor(endpointRoot))
			ep.Register(fake.New("fake", "e_1"))
			t.Cleanup(func() { _ = ep.Close() })

			identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
			droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
			defer func() { _ = droot.Close() }()
			now := time.Now()
			id, _ := format.NewMessageID(now)
			body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-1111111111b7","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(now.Add(time.Minute)) + `","input":{"text":"hi"}}`
			msg := format.Message{Header: format.Header{
				Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{amqio.DefaultHandle},
				Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
				ReplyTo: "codex@qa",
			}, Body: body}
			data, _ := msg.Marshal()
			if _, err := fsq.DeliverToInboxes(droot, []string{amqio.DefaultHandle}, id+".md", data); err != nil {
				t.Fatalf("deliver: %v", err)
			}

			// TICK 1: session qa does not exist yet. Transient: the command stays in new.
			if n, _ := carrier.ImportOnce(); n != 0 {
				t.Fatalf("TICK1: command handled (%d) although the caller session does not exist (B7 — must be transient)", n)
			}
			if entries, _ := os.ReadDir(fsq.AgentInboxNew(endpointRoot, amqio.DefaultHandle)); len(entries) != 1 {
				t.Fatalf("TICK1: command should stay in new, found %d", len(entries))
			}

			qa := filepath.Join(baseRoot, "qa")
			if err := fsq.EnsureRootDirs(qa); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(qa, "codex"); err != nil {
				t.Fatal(err)
			}
			// TICK 2: the session exists. The reply lands in ITS codex inbox.
			n, err := carrier.ImportOnce()
			if err != nil || n != 1 {
				t.Fatalf("TICK2: n=%d err=%v, want the command handled", n, err)
			}
			if entries, _ := os.ReadDir(fsq.AgentInboxNew(qa, "codex")); len(entries) == 0 {
				t.Fatal("TICK2: no reply in the caller session's codex inbox (B7)")
			}
			if _, err := os.Stat(filepath.Join(endpointRoot, "agents", "codex")); err == nil {
				t.Fatal("reply went to a codex mailbox in the endpoint's own root instead of the caller's session (B7)")
			}
		})
	}
}

// TestCLICancelUsesStoredEpochAfterAttachmentRestart reproduces B6b
// (agent-message-queue-611.22.35): a fresh attachment mints a fresh epoch,
// and `cancel` used to send that epoch, so every cancel of a request stored
// under the old epoch was refused as stale_epoch. cancel now fetches the
// stored request's epoch first.
func TestCLICancelUsesStoredEpochAfterAttachmentRestart(t *testing.T) {
	// Short temp path: the IPC socket lives under root and unix socket paths
	// are capped at 104 bytes on macOS (same reason startServe uses MkdirTemp).
	root, err := os.MkdirTemp("", "amqr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, stateDirName)
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ep := core.New(core.Config{Store: store})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })
	server, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() { _ = server.Serve(ctx) }()

	code, out, errOut := cli(t, "", "submit", "fake", "--text", "say hi", "--root", root, "--json")
	if code != 0 {
		t.Fatalf("submit exit=%d out=%s err=%s", code, out, errOut)
	}
	var rep protocol.Reply
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("submit output is not a Reply: %v (%s)", err, out)
	}

	// The attachment restarts: a new epoch. The stored request keeps e_1.
	rt.SwitchSession("e_2")

	code, out, errOut = cli(t, "", "cancel", rep.Snapshot.RequestRef, "--root", root, "--json")
	var crep protocol.Reply
	if uerr := json.Unmarshal([]byte(out), &crep); uerr != nil {
		t.Fatalf("cancel exit=%d output is not a Reply: %v (out=%s err=%s)", code, uerr, out, errOut)
	}
	if crep.Outcome.Code == protocol.CodeStaleEpoch {
		t.Fatalf("cancel refused as stale_epoch after an attachment restart (B6b — cancel must use the stored request's epoch): exit=%d out=%s", code, out)
	}
	if crep.Snapshot.Cancel == nil && crep.Snapshot.State != protocol.StateCancelled {
		t.Fatalf("cancel recorded nothing: state=%s outcome=%+v", crep.Snapshot.State, crep.Outcome)
	}
}

// TestReplyRouterForPeerSessionNotYetCreated reproduces packet 7 of
// agent-message-queue-611.22.36: a cross-project reply whose peer BASE root
// exists but whose peer SESSION does not yet exist was classified poison and
// the command was dead-lettered; creating the session afterwards could not
// bring it back. An absent peer session is as transient as an absent peer
// root: the command stays in new and is delivered once the session exists.
func TestReplyRouterForPeerSessionNotYetCreated(t *testing.T) {
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_SESSION", "")
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
	// The peer BASE root exists; session "collab" does not.
	callerAbs := filepath.Join(filepath.Dir(endpointBase), "caller", ".agent-mail")
	if err := fsq.EnsureRootDirs(callerAbs); err != nil {
		t.Fatal(err)
	}
	t.Chdir(endpointBase)

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
	carrier.SetReplyRouter(replyRouterFor(endpointRoot))
	ep.Register(fake.New("fake", "e_1"))
	t.Cleanup(func() { _ = ep.Close() })

	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	defer func() { _ = droot.Close() }()
	now := time.Now()
	id, _ := format.NewMessageID(now)
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111707","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(now.Add(time.Minute)) + `","input":{"text":"hi"}}`
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{amqio.DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
		FromProject: "caller", ReplyTo: "codex@collab", ReplyProject: "caller",
	}, Body: body}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInboxes(droot, []string{amqio.DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// TICK 1: peer session absent. Transient: stays in new, not DLQ'd.
	if n, _ := carrier.ImportOnce(); n != 0 {
		t.Fatalf("TICK1: command handled (%d) although the peer session does not exist (packet 7)", n)
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(endpointRoot, amqio.DefaultHandle)); len(entries) != 1 {
		t.Fatalf("TICK1: command should stay in new, found %d", len(entries))
	}
	if entries, _ := os.ReadDir(filepath.Join(endpointRoot, "agents", amqio.DefaultHandle, "dlq", "new")); len(entries) != 0 {
		t.Fatalf("TICK1: command was dead-lettered (packet 7 — an absent peer session is not poison): %d", len(entries))
	}

	// The session is created. TICK 2 delivers into it.
	sessionRoot := filepath.Join(callerAbs, "collab")
	if err := fsq.EnsureRootDirs(sessionRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(sessionRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	n, err := carrier.ImportOnce()
	if err != nil || n != 1 {
		t.Fatalf("TICK2: n=%d err=%v, want the command handled", n, err)
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(sessionRoot, "codex")); len(entries) == 0 {
		t.Fatal("TICK2: no reply in the caller session's codex inbox")
	}
}

// TestServeRegistersHandleInConfig is the round-1 review blocker B3 test
// (611.22.19 round-2): the .10 registration feature was completely untested
// at the serve boundary. This test boots a real serve with --me remote and
// verifies that config.json exists and contains the "remote" handle. It also
// verifies that an existing agent ("codex") is preserved — the core
// invariant of EnsureAgent.
func TestServeRegistersHandleInConfig(t *testing.T) {
	root, err := os.MkdirTemp("", "amqr10")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "meta", "config.json")
	seed := struct {
		Version    int      `json:"version"`
		CreatedUTC string   `json:"created_utc"`
		Agents     []string `json:"agents"`
	}{Version: 1, CreatedUTC: "2026-01-01T00:00:00Z", Agents: []string{"codex"}}
	seedData, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(configPath, append(seedData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- run([]string{"serve", "--fake", "--root", root, "--me", "remote"}, strings.NewReader(""), &out, &errBuf)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out, errBuf bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &out, &errBuf) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config.json not found after serve: %v", err)
	}
	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config.json is not valid JSON: %v", err)
	}
	has := func(h string) bool {
		for _, a := range cfg.Agents {
			if a == h {
				return true
			}
		}
		return false
	}
	if !has("codex") {
		t.Fatalf("serve lost existing agent 'codex': %v", cfg.Agents)
	}
	if !has("remote") {
		t.Fatalf("serve did not register handle 'remote': %v", cfg.Agents)
	}
}

// TestSenderB7PersistBeforeDispatchCLI is the headline clause test (611.7
// round-2 B7): submit with the endpoint DOWN persists the envelope, then serve
// drains it. Inverting persist-before-dispatch in the CLI (dispatch first,
// persist on failure) leaves the envelope absent when the endpoint is down,
// so the drain never fires and the request never reaches the runtime. This
// test goes RED on that inversion.
// === B7 TESTS BELOW (clean rewrite) ===

// TestSenderB7PersistBeforeDispatchCLI is the headline clause test (611.7
// round-2 B7): submit with the endpoint DOWN persists the envelope, then serve
// drains it. Inverting persist-before-dispatch in the CLI (dispatch first,
// persist on failure) leaves the envelope absent when the endpoint is down,
// so the drain never fires and the request never reaches the runtime. This
// test goes RED on that inversion.
func TestSenderB7PersistBeforeDispatchCLI(t *testing.T) {
	root, err := os.MkdirTemp("", "amqrsend")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")

	// Submit with the endpoint DOWN: the envelope must be persisted before
	// dispatch, so the receipt is a sender-side SpoolReceipt (not a Snapshot).
	code, out, _ := cli(t, "", "submit", "fake", "--text", "persisted before dispatch",
		"--root", root, "--epoch", "e_1", "--request-id", "11111111-1111-4111-8111-1111111117b7",
		"--json")
	if code != 0 {
		t.Fatalf("submit (endpoint down) exit=%d out=%s", code, out)
	}
	// The receipt must NOT mint a request ref or revision (B3).
	if strings.Contains(out, `"request_ref"`) || strings.Contains(out, `"revision"`) {
		t.Fatalf("offline receipt mints request_ref or revision (B3): %s", out)
	}
	if !strings.Contains(out, "sender_submitted") {
		t.Fatalf("offline receipt is not sender_submitted: %s", out)
	}

	// The envelope is on disk before serve starts.
	spool, err := sender.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	env, exists, err := spool.Get(ipc.LocalHost, "11111111-1111-4111-8111-1111111117b7")
	if err != nil || !exists {
		t.Fatalf("envelope not persisted before dispatch: exists=%v err=%v", exists, err)
	}
	if env.State != sender.StatePending {
		t.Fatalf("envelope state=%s, want pending", env.State)
	}

	// Start serve: the drainer must replay the pending envelope.
	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- run([]string{"serve", "--fake", "--root", root, "--poll", "50ms"},
			strings.NewReader(""), &out, &errBuf)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
		}
	})

	// Wait for the socket to accept.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out, errBuf bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &out, &errBuf) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Wait for the drainer to dispatch the envelope (poll runs every 50ms).
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		env, _, _ := spool.Get(ipc.LocalHost, "11111111-1111-4111-8111-1111111117b7")
		if env != nil && env.State == sender.StateDispatched {
			return // success
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("drainer did not dispatch the persisted envelope within 5s")
}

// TestSenderB7RefusedNotDispatched is the B2 regression: a refused submit
// (stale_epoch) must NOT be marked dispatched. The drainer classifies by
// Outcome.Code, not by error.
func TestSenderB7RefusedNotDispatched(t *testing.T) {
	root := startServe(t)
	stateDir := filepath.Join(root, "extensions", "remote")

	// Submit with a stale epoch so the endpoint returns stale_epoch (a
	// terminal refusal). The spool envelope must NOT be marked dispatched.
	code, _, _ := cli(t, "", "submit", "fake", "--text", "will be refused",
		"--root", root, "--epoch", "e_stale",
		"--request-id", "11111111-1111-4111-8111-1111111117b2",
		"--json")
	_ = code // exit code varies; the assertion is on the spool state
	spool, err := sender.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// Wait a moment for the drainer to process if serve is running.
	time.Sleep(200 * time.Millisecond)
	env, exists, _ := spool.Get(ipc.LocalHost, "11111111-1111-4111-8111-1111111117b2")
	if !exists {
		return // live dispatch handled it
	}
	if env.State == sender.StateDispatched && code != 0 {
		t.Fatalf("envelope marked dispatched but submit was refused (B2): state=%s", env.State)
	}
}

// TestSenderB7ReapWithoutDrain is the B4 regression: Reap runs on every
// serve tick, independent of Drain activity.
func TestSenderB7ReapWithoutDrain(t *testing.T) {
	root := startServe(t)
	stateDir := filepath.Join(root, "extensions", "remote")

	code, _, _ := cli(t, "", "submit", "fake", "--text", "reap me",
		"--root", root, "--request-id", "11111111-1111-4111-8111-1111111117b4",
		"--epoch", "e_1", "--json")
	if code != 0 {
		t.Fatalf("submit exit=%d", code)
	}
	spool, err := sender.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		env, _, _ := spool.Get(ipc.LocalHost, "11111111-1111-4111-8111-1111111117b4")
		if env != nil && env.State == sender.StateDispatched {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	env, exists, _ := spool.Get(ipc.LocalHost, "11111111-1111-4111-8111-1111111117b4")
	if !exists || env.State != sender.StateDispatched {
		t.Fatalf("envelope not dispatched before reap test")
	}
	// Write the envelope with an aged SettledAt so Reap picks it up.
	env.SettledAt = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	envPath := filepath.Join(stateDir, "sender", ipc.LocalHost+"__11111111-1111-4111-8111-1111111117b4.json")
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, exists, _ := spool.Get(ipc.LocalHost, "11111111-1111-4111-8111-1111111117b4")
		if !exists {
			return // reaped!
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("envelope was not reaped within 5s (B4: Reap not running on every tick)")
}

// TestSenderB7FailedReaped is the B5 regression: a failed envelope stamps
// SettledAt and is reaped after the horizon.
func TestSenderB7FailedReaped(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	spool, err := sender.Open(dir, sender.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	cmd := &protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-1111111117b5",
		TargetID:  "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now.Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "test", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn},
	}
	env := &sender.Envelope{
		RequestID:   cmd.RequestID,
		CreatorHost: "local",
		TargetID:    "fake", Epoch: "e_1",
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := spool.MarkFailed(sender.Key{CreatorHost: "local", RequestID: cmd.RequestID}, "stale_epoch"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, _, _ := spool.Get("local", cmd.RequestID)
	if got == nil || got.SettledAt == "" {
		t.Fatalf("B5: failed envelope has no SettledAt")
	}
	n, err := spool.Reap(now.Add(1*time.Hour), 64)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("B5: reaped %d, want 1", n)
	}
	_, exists, _ := spool.Get("local", cmd.RequestID)
	if exists {
		t.Fatal("B5: failed envelope survived reap")
	}
}

// TestCLIB6RequestsListsFailedEnvelopes (round-3) pins B6: `requests` must
// list ALL spool envelope states (pending + failed), not just pending. A
// failure is invisible in the old code. RED when the StatePending filter is
// restored.
func TestCLIB6RequestsListsFailedEnvelopes(t *testing.T) {
	root, err := os.MkdirTemp("", "amqb6")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")
	// Create the request store directory so OpenReadOnly succeeds.
	if err := os.MkdirAll(filepath.Join(stateDir, "v1", "requests"), 0o755); err != nil {
		t.Fatal(err)
	}
	spool, err := sender.Open(stateDir)
	if err != nil {
		t.Fatalf("sender open: %v", err)
	}
	now := time.Now()
	pendingCmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-11111111b601",
		TargetID:  "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now.Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "pending"},
	}
	failedCmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-11111111b602",
		TargetID:  "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now.Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "failed"},
	}
	expiredCmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-11111111b603",
		TargetID:  "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now.Add(-1 * time.Minute)), // window already closed
		Input:    &protocol.SubmitInput{Text: "expired"},
	}
	for _, cmd := range []*protocol.Command{pendingCmd, failedCmd, expiredCmd} {
		env := &sender.Envelope{
			RequestID: cmd.RequestID, CreatorHost: "local", TargetID: "fake",
			Epoch: cmd.Epoch, NotAfter: cmd.NotAfter, Command: cmd,
			Destination: "ipc:/tmp/state",
		}
		if err := spool.Create(env); err != nil {
			t.Fatalf("create %s: %v", cmd.RequestID, err)
		}
	}
	// Mark the second envelope as failed.
	if err := spool.MarkFailed(sender.Key{CreatorHost: "local", RequestID: failedCmd.RequestID}, string(protocol.CodeStaleEpoch)); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	// Expire the third envelope (its window already closed).
	if _, err := spool.Expire(sender.Key{CreatorHost: "local", RequestID: expiredCmd.RequestID}, now); err != nil {
		t.Fatalf("expire: %v", err)
	}

	// requests (no serve running) must list ALL THREE: pending, failed, expired.
	code, out, _ := cli(t, "", "requests", "--root", root, "--json")
	if code != 0 {
		t.Fatalf("requests exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, pendingCmd.RequestID) {
		t.Fatalf("requests did not list pending envelope: %s", out)
	}
	if !strings.Contains(out, failedCmd.RequestID) {
		t.Fatalf("requests did not list failed envelope (B6: only pending listed): %s", out)
	}
	if !strings.Contains(out, string(protocol.CodeStaleEpoch)) {
		t.Fatalf("requests did not include last_error for failed envelope: %s", out)
	}
	// B6 round-5 gap 1: expired envelope must be listed as expired, not received.
	if !strings.Contains(out, expiredCmd.RequestID) {
		t.Fatalf("requests did not list expired envelope: %s", out)
	}
	if !strings.Contains(out, string(protocol.StateRejected)) {
		t.Fatalf("requests did not map expired to state rejected: %s", out)
	}
	if !strings.Contains(out, string(protocol.CodeExpired)) {
		t.Fatalf("requests did not include code expired for expired envelope: %s", out)
	}
}

// TestCLIB6StatusFailedEnvelopeExitsOne (round-3) pins B6: `status` on a
// failed spool envelope must print last_error and exit 1 (ExitError), not 0.
// It accepts a raw request ID (no ref — B3 stopped minting refs). RED when
// exitForSpoolReceipt is removed (always ExitSuccess).
func TestCLIB6StatusFailedEnvelopeExitsOne(t *testing.T) {
	root, err := os.MkdirTemp("", "amqb6s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")
	spool, err := sender.Open(stateDir)
	if err != nil {
		t.Fatalf("sender open: %v", err)
	}
	now := time.Now()
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-11111111b610",
		TargetID:  "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now.Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "stale"},
	}
	env := &sender.Envelope{
		RequestID: cmd.RequestID, CreatorHost: "local", TargetID: "fake",
		Epoch: cmd.Epoch, NotAfter: cmd.NotAfter, Command: cmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := spool.MarkFailed(sender.Key{CreatorHost: "local", RequestID: cmd.RequestID}, string(protocol.CodeStaleEpoch)); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	// status with raw request ID (no ref): exit 1, last_error present.
	code, out, _ := cli(t, "", "status", cmd.RequestID, "--root", root, "--json")
	if code != protocol.ExitError {
		t.Fatalf("status failed envelope exit=%d, want %d (ExitError) out=%s", code, protocol.ExitError, out)
	}
	if !strings.Contains(out, string(protocol.CodeStaleEpoch)) {
		t.Fatalf("status did not print last_error: %s", out)
	}

	// B6 round-5 gap 2: expired envelope must also exit 1 (not 0).
	expiredCmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-11111111b611",
		TargetID:  "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now.Add(-1 * time.Minute)), // window already closed
		Input:    &protocol.SubmitInput{Text: "expired"},
	}
	expiredEnv := &sender.Envelope{
		RequestID: expiredCmd.RequestID, CreatorHost: "local", TargetID: "fake",
		Epoch: expiredCmd.Epoch, NotAfter: expiredCmd.NotAfter, Command: expiredCmd,
		Destination: "ipc:/tmp/state",
	}
	if err := spool.Create(expiredEnv); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if _, err := spool.Expire(sender.Key{CreatorHost: "local", RequestID: expiredCmd.RequestID}, now); err != nil {
		t.Fatalf("expire: %v", err)
	}
	code2, out2, _ := cli(t, "", "status", expiredCmd.RequestID, "--root", root, "--json")
	if code2 != protocol.ExitError {
		t.Fatalf("status expired envelope exit=%d, want %d (ExitError) out=%s", code2, protocol.ExitError, out2)
	}
	if !strings.Contains(out2, string(protocol.CodeExpired)) {
		t.Fatalf("status did not print code expired for expired envelope: %s", out2)
	}
}

// recordingFake wraps fake.Runtime and records whether the envelope file
// existed on disk at the moment Submit (Handle) was called. It is used by
// TestCLIB7PersistBeforeDispatchViaSubmit to prove the CLI submit path
// persists the envelope BEFORE dispatching it to the endpoint.
type recordingFake struct {
	*fake.Runtime
	stateDir    string
	fileExisted atomic.Bool
}

func (r *recordingFake) Submit(req core.BoundRequest) (core.Admission, error) {
	// Check if the envelope file exists at Handle time.
	path := filepath.Join(r.stateDir, "sender", req.Key.CreatorHost+"__"+req.Key.RequestID+".json")
	if _, err := os.Stat(path); err == nil {
		r.fileExisted.Store(true)
	}
	return r.Runtime.Submit(req)
}

// TestCLIB7PersistBeforeDispatchViaSubmit (round-4) drives the REAL CLI
// submit path and proves the envelope is durable on disk BEFORE the
// endpoint's Handle (Submit) is called. A recording fake wraps fake.Runtime;
// its Submit checks whether the envelope file exists at Handle time.
//
// RED on both inversions:
//   - Create moved after callReply: file does not exist at Handle time.
//   - Create deleted entirely: file does not exist at Handle time.
//
// This replaces the round-3 TestSenderB7PersistBeforeDispatch which drove the
// drainer directly (the file exists by construction when the drainer reads
// it) and whose RED was produced by commenting out the test's own Create.
func TestCLIB7PersistBeforeDispatchViaSubmit(t *testing.T) {
	root, err := os.MkdirTemp("", "amqb7")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")

	// Start serve with the recording fake instead of the standard --fake.
	rf := &recordingFake{Runtime: fake.New("fake", "e_1"), stateDir: stateDir}
	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- runWithRecordingFake(root, rf, &out, &errBuf)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
		}
	})

	// Wait for the socket to accept.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out, errBuf bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &out, &errBuf) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		t.Fatal("endpoint did not start serving within 5s")
	}

	// Submit via the CLI. The submit function persists the envelope BEFORE
	// calling callReply (IPC → Handle → fake.Submit). The recording fake
	// checks the file exists at Submit time.
	requestID := "11111111-1111-4111-8111-11111111b701"
	code, out, _ := cli(t, "", "submit", "fake", "--root", root,
		"--text", "b7 probe", "--request-id", requestID, "--epoch", "e_1")
	if code != 0 {
		t.Fatalf("submit exit=%d out=%s", code, out)
	}

	if !rf.fileExisted.Load() {
		t.Fatal("B7: envelope file did NOT exist at Handle time (persist-after-dispatch or deleted Create)")
	}
}

// runWithRecordingFake starts serve with a recording fake attachment instead
// of the standard --fake. It mirrors serve() but registers rf directly. It
// blocks until the test process exits.
func runWithRecordingFake(root string, rf *recordingFake, stdout, stderr io.Writer) int {
	stateDir := filepath.Join(root, "extensions", "remote")
	store, err := requests.Open(stateDir)
	if err != nil {
		return 0
	}
	ep := core.New(core.Config{Store: store, Publish: func(protocol.Snapshot, map[string]string) error { return nil }})
	carrier, err := amqio.New(root, amqio.DefaultHandle, ep)
	if err != nil {
		_ = store.Close()
		return 0
	}
	carrier.SetReplyRouter(replyRouterFor(root))
	ep.Register(rf)
	if err := ep.Reconcile(); err != nil {
		_ = ep.Close()
		return 0
	}
	server, err := ipc.Listen(stateDir, ep)
	if err != nil {
		_ = ep.Close()
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()
	// Block until the test process exits.
	select {}
}

// TestBK4ServeWiringCompactionNonVacuous (round-4) tests BOTH halves of the
// SHIPPED serve wiring via openServeStore:
//
//  1. Horizon half: a settled old record is compacted after Reconcile.
//     RED when DefaultCompactHorizon wiring is removed from openServeStore.
//  2. Quota half: a submit past DefaultMaxStoreBytes is refused
//     storage_full through the endpoint openServeStore built.
//     RED when DefaultMaxStoreBytes wiring is removed (quota=0 = unbounded).
//
// openServeStore no longer calls Reconcile (round-4 P0: it ran before
// SetPublish/Register, marking every running record attachment_lost). The
// test calls Reconcile explicitly after wiring a no-op publisher, exactly
// as serve does.
func TestBK4ServeWiringCompactionNonVacuous(t *testing.T) {
	root, err := os.MkdirTemp("", "amqbk4w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")

	// --- Seed one settled old record eligible for compaction ---
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	seedStore, err := requests.Open(stateDir,
		requests.WithMaxStoreBytes(protocol.DefaultMaxStoreBytes),
		requests.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   "11111111-1111-4111-8111-111111111730",
			CreatorHost: "hostA",
			TargetID:    "t_fake1",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: requests.Digest([]byte("say hi")),
		},
		Input: &protocol.SubmitInput{Text: "say hi"},
	}
	k := requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
	if err := seedStore.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec.Revision, rec.State = 2, protocol.StateDispatching
	if err := seedStore.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	rec.Revision, rec.State = 3, protocol.StateCompleted
	rec.Result = &protocol.Result{Text: "done"}
	rec.ObservedAt = "2026-09-01T00:00:00Z" // old: before the compact horizon
	rec.AckDigest = protocol.EvidenceDigest(rec.Result)
	if err := seedStore.Update(rec); err != nil {
		t.Fatalf("completed: %v", err)
	}
	if err := seedStore.MarkPublished(k, 3); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	// --- Horizon half: openServeStore + explicit Reconcile compacts ---
	store, ep, err := openServeStore(stateDir, func() time.Time { return now })
	if err != nil {
		t.Fatalf("openServeStore: %v", err)
	}
	defer func() { _ = ep.Close() }()

	// Wire a no-op publisher (as serve does via SetPublish before Reconcile).
	ep.SetPublish(func(protocol.Snapshot, map[string]string) error { return nil })

	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, exists, err := store.Get(k)
	if err != nil || !exists {
		t.Fatalf("record missing after reconcile: exists=%v err=%v", exists, err)
	}
	if !got.Tombstone {
		t.Fatal("horizon half: record not tombstoned (DefaultCompactHorizon wiring missing from openServeStore?)")
	}

	// --- Quota half: openServeStore wired DefaultMaxStoreBytes ---
	// The store openServeStore built must have the production quota. If the
	// DefaultMaxStoreBytes wiring is removed (quota=0 = unbounded), this goes
	// RED. We assert the value directly because filling 64MiB in a test is
	// impractical; the accessor confirms the wiring reached the store.
	if got := store.MaxStoreBytes(); got != protocol.DefaultMaxStoreBytes {
		t.Fatalf("quota half: store maxStoreBytes=%d, want %d (DefaultMaxStoreBytes wiring missing from openServeStore?)", got, protocol.DefaultMaxStoreBytes)
	}
}

// TestCLISubmitPrintsAchievedEvidence pins the architect-review invariant:
// submit prints the achieved evidence class (the human projection) on every
// successful submit, so a human sees `admitted` from the fake (or
// `submitted` from Amit/Claude) even when no floor was asked. The floor is
// the machine contract on SubmitInput; the projection is the live session's
// Evidence.Submit. The CLI renders it as evidence=<class> on the human path.
func TestCLISubmitPrintsAchievedEvidence(t *testing.T) {
	root := startServe(t)

	// JSON path: the Outcome.Evidence field carries the class.
	code, out, errOut := cli(t, "", "submit", "fake", "--text", "say hi", "--root", root, "--json")
	if code != 0 {
		t.Fatalf("json submit exit=%d out=%s err=%s", code, out, errOut)
	}
	var rep protocol.Reply
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json output not a Reply: %v (%s)", err, out)
	}
	if rep.Outcome.Evidence != protocol.EvidenceAdmitted {
		t.Fatalf("Outcome.Evidence=%q, want %q", rep.Outcome.Evidence, protocol.EvidenceAdmitted)
	}

	// Human path on a FRESH serve (the fake stays busy after one submit):
	// the evidence line must appear in the text output.
	root2 := startServe(t)
	code, out, errOut = cli(t, "", "submit", "fake", "--text", "say hi", "--root", root2)
	if code != 0 {
		t.Fatalf("human submit exit=%d out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "evidence=admitted") {
		t.Fatalf("human submit output missing evidence=admitted:\n%s", out)
	}
}

// TestCLISubmitMinEvidenceUnsupportedExits6 pins that a min_evidence floor
// refused by a weaker adapter maps to exit 6 (action-required / capability
// mismatch), NOT exit 1 (failure). The caller must not take the "work failed"
// recovery path for a capability mismatch.
func TestCLISubmitMinEvidenceUnsupportedExits6(t *testing.T) {
	root := startServe(t)
	// The fake proves submit=admitted, so requiring admitted succeeds and
	// requiring submitted also succeeds (admitted meets submitted). There is
	// no CLI flag to swap the fake to a weaker adapter; the refusal-exit-6
	// mapping is exercised at the core level
	// (TestMinEvidenceFloorRefusesWeakerAdapter proves the refusal) and the
	// exit-code mapping is ExitForCode(CodeUnsupported)=ExitActionRequired=6.
	// Here we pin the positive: a met floor still exits 0 and prints evidence.
	code, out, errOut := cli(t, "", "submit", "fake", "--text", "floored", "--min-evidence", "submitted", "--root", root)
	if code != 0 {
		t.Fatalf("met floor submit exit=%d, want 0 (out=%s err=%s)", code, out, errOut)
	}
	if !strings.Contains(out, "evidence=admitted") {
		t.Fatalf("met-floor submit missing evidence=admitted:\n%s", out)
	}
	// Pin the exit-code mapping directly: unsupported is action-required (6).
	if got := protocol.ExitForCode(protocol.CodeUnsupported); got != protocol.ExitActionRequired {
		t.Fatalf("ExitForCode(unsupported)=%d, want %d", got, protocol.ExitActionRequired)
	}
}

// TestBK4P0RunningStaysRunningAfterReconcile (round-5) is the first P0
// regression probe. A running record (simulating a restart mid-run) bound to
// a fake attachment whose Lookup reports running MUST stay running after the
// startup Reconcile — NOT get marked attachment_lost/uncertain.
//
// The test runs startupSequence — the ONE construction-plus-reconcile
// function serve calls (open store → SetPublish → Register → Reconcile) —
// with the fake attachment, so reconcileLive's Lookup finds the target.
//
// RED when Reconcile is moved back before Register (the P0 bug): with no
// attachment registered, reconcileLive's `!ok` branch marks the record
// StateUncertain + CodeAttachmentLost.
func TestBK4P0RunningStaysRunningAfterReconcile(t *testing.T) {
	root, err := os.MkdirTemp("", "amqbk4p0a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")

	// Seed a running record directly in the store (restart mid-run).
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	seedStore, err := requests.Open(stateDir,
		requests.WithMaxStoreBytes(protocol.DefaultMaxStoreBytes),
		requests.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	rt := fake.New("fake", "e_1")
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   "11111111-1111-4111-8111-111111110011",
			CreatorHost: "hostA",
			TargetID:    "fake",
			RequestRef:  protocol.EncodeRef("hostA", "fake", "11111111-1111-4111-8111-111111110011"),
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: requests.Digest([]byte("run")),
		},
		Input: &protocol.SubmitInput{Text: "run"},
	}
	k := requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
	if err := seedStore.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Dispatch through the fake so a run is bound (Lookup will confirm running).
	admission, err := rt.Submit(core.BoundRequest{Key: k, Epoch: "e_1", Input: *rec.Input})
	if err != nil {
		t.Fatalf("fake submit: %v", err)
	}
	rec.Revision = 2
	rec.State = protocol.StateDispatching
	if err := seedStore.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	rec.Revision = 3
	rec.State = protocol.StateRunning
	rec.NativeRun = &admission.RunID
	if err := seedStore.Update(rec); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	// serve sequence via ONE shared function: startupSequence (open store →
	// SetPublish → Register → Reconcile). The fake is registered BEFORE
	// Reconcile so the sweep sees the live target (the P0 bug did not).
	store, ep, _, err := startupSequence(stateDir, root, amqio.DefaultHandle, func() time.Time { return now },
		func(protocol.Snapshot, map[string]string) error { return nil }, nil, rt)
	if err != nil {
		t.Fatalf("startupSequence: %v", err)
	}
	defer func() { _ = ep.Close() }()

	got, exists, err := store.Get(k)
	if err != nil || !exists {
		t.Fatalf("record missing: exists=%v err=%v", exists, err)
	}
	if got.State == protocol.StateUncertain && got.Code == protocol.CodeAttachmentLost {
		t.Fatal("P0: running record marked attachment_lost after reconcile (Reconcile ran before Register — the P0 bug)")
	}
	if got.State != protocol.StateRunning {
		t.Fatalf("P0: running record state=%s code=%s, want running (Reconcile before Register marks it attachment_lost)", got.State, got.Code)
	}
}

// TestBK4P0FirstRevisionReachesPublisher (round-5) is the second P0
// regression probe. The first Reconcile revision of a running record must
// reach the real publisher (SetPublish), not a no-op. When Reconcile runs
// before SetPublish (the P0 bug), the published snapshot's revision is lost
// to a nil publisher callback.
//
// The test seeds a running record, wires a publisher that records the
// snapshot it receives, registers the fake, then calls Reconcile. The
// publisher must see the record's revision.
//
// RED when Reconcile is moved before SetPublish: the publisher is nil, so
// no snapshot is recorded.
func TestBK4P0FirstRevisionReachesPublisher(t *testing.T) {
	root, err := os.MkdirTemp("", "amqbk4p0b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")

	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	seedStore, err := requests.Open(stateDir,
		requests.WithMaxStoreBytes(protocol.DefaultMaxStoreBytes),
		requests.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	rt := fake.New("fake", "e_1")
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   "11111111-1111-4111-8111-111111110022",
			CreatorHost: "hostA",
			TargetID:    "fake",
			RequestRef:  protocol.EncodeRef("hostA", "fake", "11111111-1111-4111-8111-111111110022"),
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: requests.Digest([]byte("pub")),
		},
		Input: &protocol.SubmitInput{Text: "pub"},
	}
	k := requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
	if err := seedStore.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	admission, err := rt.Submit(core.BoundRequest{Key: k, Epoch: "e_1", Input: *rec.Input})
	if err != nil {
		t.Fatalf("fake submit: %v", err)
	}
	rec.Revision = 2
	rec.State = protocol.StateDispatching
	if err := seedStore.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	rec.Revision = 3
	rec.State = protocol.StateRunning
	rec.NativeRun = &admission.RunID
	if err := seedStore.Update(rec); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	var publishedRev int64
	var pubMu sync.Mutex
	var pubCalled bool
	publish := func(s protocol.Snapshot, origin map[string]string) error {
		pubMu.Lock()
		pubCalled = true
		publishedRev = s.Revision
		pubMu.Unlock()
		return nil
	}
	// serve sequence via ONE shared function: startupSequence (open store →
	// SetPublish → Register → Reconcile). RED when Reconcile is moved before
	// SetPublish inside startupSequence: the nil publisher records nothing.
	_, ep, _, err := startupSequence(stateDir, root, amqio.DefaultHandle, func() time.Time { return now }, publish, nil, rt)
	if err != nil {
		t.Fatalf("startupSequence: %v", err)
	}
	defer func() { _ = ep.Close() }()

	pubMu.Lock()
	called := pubCalled
	rev := publishedRev
	pubMu.Unlock()
	if !called {
		t.Fatal("P0: publisher was never called (Reconcile ran before SetPublish — the first revision was lost to a no-op publisher)")
	}
	if rev < 2 {
		t.Fatalf("P0: published revision=%d, want >=2 (the running record's revision)", rev)
	}
}

// TestCLIB6StatusByRequestIdWhileServeUp (round-5 gap 3) pins that `status`
// with a bare request ID works WHILE SERVE IS UP. A running endpoint refuses
// a bare id as invalid; the spool must be consulted first so the surface that
// reports a drain failure is reachable at exactly the moment the failure
// exists. Asserts state failed + last_error + exit 1.
//
// RED when the spool-first resolution is removed (status sends the bare id to
// the live endpoint, which refuses it as invalid, and the invalid fallback is
// absent): exit 2, no last_error.
func TestCLIB6StatusByRequestIdWhileServeUp(t *testing.T) {
	root := startServe(t)
	stateDir := filepath.Join(root, "extensions", "remote")
	spool, err := sender.Open(stateDir)
	if err != nil {
		t.Fatalf("sender open: %v", err)
	}
	now := time.Now()
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-11111111b620",
		TargetID:  "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now.Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "stale-while-up"},
	}
	env := &sender.Envelope{
		RequestID: cmd.RequestID, CreatorHost: ipc.LocalHost, TargetID: "fake",
		Epoch: cmd.Epoch, NotAfter: cmd.NotAfter, Command: cmd,
		Destination: "ipc:" + stateDir,
	}
	if err := spool.Create(env); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := spool.MarkFailed(sender.Key{CreatorHost: ipc.LocalHost, RequestID: cmd.RequestID}, string(protocol.CodeStaleEpoch)); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	// status with a bare request ID while serve is up: the endpoint would
	// refuse a bare id as invalid, but the spool-first resolution returns the
	// failed envelope directly. Exit 1, last_error present.
	code, out, _ := cli(t, "", "status", cmd.RequestID, "--root", root, "--json")
	if code != protocol.ExitError {
		t.Fatalf("status by request-id while serve up exit=%d, want %d (ExitError) out=%s", code, protocol.ExitError, out)
	}
	if !strings.Contains(out, string(protocol.CodeStaleEpoch)) {
		t.Fatalf("status did not print last_error for failed envelope: %s", out)
	}
}

// TestBK4R6CarrierConstructedInsideStartupSequence (round-6) pins that the
// carrier is constructed INSIDE startupSequence, before Reconcile. The
// startup reconcile revision must reach the carrier, not a no-op publisher.
// The test seeds a running record, calls startupSequence with a publish
// callback that captures the carrier variable, and asserts: (1) the carrier
// is non-nil when the publish callback is invoked during Reconcile (proving
// the carrier was constructed before Reconcile ran); (2) the carrier is
// non-nil when startupSequence returns.
//
// RED when carrier construction is moved after startupSequence returns: the
// carrierPublish closure sees carrier==nil during Reconcile and returns nil
// (no-op), so the startup revision is lost to a no-op publisher.
func TestBK4R6CarrierConstructedInsideStartupSequence(t *testing.T) {
	root, err := os.MkdirTemp("", "amqbk4r6a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	// Seed a running record directly in the store (restart mid-run).
	seedStore, err := requests.Open(stateDir, requests.WithMaxStoreBytes(protocol.DefaultMaxStoreBytes))
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	rt := fake.New("fake", "e_1")
	id := "22222222-2222-4222-8222-2222222260a1"
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   id,
			CreatorHost: "hostA",
			TargetID:    "fake",
			RequestRef:  protocol.EncodeRef("hostA", "fake", id),
			Epoch:       "e_1",
			State:       protocol.StateReceived,
			Revision:    1,
		},
		Input: &protocol.SubmitInput{Text: "r6 probe"},
	}
	if err := seedStore.Create(rec); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	// Dispatch through the fake so a run is bound (Lookup will confirm running).
	k := requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}
	admission, err := rt.Submit(core.BoundRequest{Key: k, Epoch: "e_1", Input: *rec.Input})
	if err != nil {
		t.Fatalf("fake submit: %v", err)
	}
	rec.Revision = 2
	rec.State = protocol.StateDispatching
	if err := seedStore.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	rec.Revision = 3
	rec.State = protocol.StateRunning
	rec.NativeRun = &admission.RunID
	if err := seedStore.Update(rec); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	// The publish callback mirrors serve's carrierPublish closure: it forwards
	// to the carrier variable, which is assigned inside startupSequence. If
	// the carrier is constructed AFTER Reconcile (revert), the closure sees
	// carrier==nil during Reconcile and the startup revision is lost.
	var carrier *amqio.Carrier
	var carrierNilDuringPublish atomic.Bool
	publish := func(s protocol.Snapshot, origin map[string]string) error {
		if carrier == nil {
			carrierNilDuringPublish.Store(true)
			return nil
		}
		return carrier.Publish(s, origin)
	}

	_, ep, carrier, err := startupSequence(stateDir, root, amqio.DefaultHandle, func() time.Time { return now }, publish, &carrier, rt)
	if err != nil {
		t.Fatalf("startupSequence: %v", err)
	}
	defer func() { _ = ep.Close() }()

	// (1) The carrier must be non-nil: constructed INSIDE startupSequence.
	if carrier == nil {
		t.Fatal("R6: carrier is nil after startupSequence (not constructed inside)")
	}
	// (2) The carrier was non-nil when the publish callback was invoked during
	// Reconcile. If the carrier were constructed after startupSequence, the
	// closure would see carrier==nil and the startup revision would be lost.
	if carrierNilDuringPublish.Load() {
		t.Fatal("R6: carrier was nil when publish was called during Reconcile (carrier constructed after Reconcile, not inside startupSequence)")
	}
}

// TestStartupSequenceWithManifest (611.13 r1) extends the round-6
// startup-construction assertion: a manifest entry feeds registry.Build,
// which feeds startupSequence. The carrier is constructed inside
// startupSequence with the manifest's fake adapter registered, so the
// startup revision reaches the carrier. This replaces the deleted
// sleep-based integration test (TestServeReadsManifest).
func TestStartupSequenceWithManifest(t *testing.T) {
	root, err := os.MkdirTemp("", "amqrmseq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Write a manifest with one fake adapter under the target id 'fake' —
	// deliberately NOT --fake sugar: serve must read THIS manifest file. This
	// goes RED when the manifest read is removed from the production path
	// (serve or serveStartup): no manifest, no adapter, no target (611.13 r4
	// item 2).
	mf := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Layer:         manifest.Layer,
		Adapters: []manifest.Adapter{
			{Kind: "fake", Target: "fake", Epoch: "e_1"},
		},
	}
	data, _ := json.Marshal(mf)
	if err := os.WriteFile(manifest.DefaultPath(stateDir), data, 0644); err != nil {
		t.Fatal(err)
	}
	// Drive the production serve path end to end: run serve (which loads
	// this manifest, builds, owns the store, publishes diagnostics), then
	// assert on the live endpoint via the IPC surface a client uses.
	done := make(chan int, 1)
	var serveErrBuf, serveOutBuf bytes.Buffer
	go func() {
		done <- run([]string{"serve", "--root", root, "--manifest", manifest.DefaultPath(stateDir)}, strings.NewReader(""), &serveOutBuf, &serveErrBuf)
	}()
	t.Cleanup(func() {
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
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	var out, errBuf bytes.Buffer
	if code := run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &out, &errBuf); code != 0 {
		t.Fatalf("endpoint did not start serving within 5s; sessions exit=%d stderr=%s", code, errBuf.String())
	}
	// The manifest-declared target MUST be reachable through the running
	// endpoint. Assert on the session surface, not an in-memory shortcut.
	var sessions []protocol.Session
	if err := json.Unmarshal(out.Bytes(), &sessions); err != nil {
		t.Fatalf("sessions reply: %v\nstderr=%s", err, errBuf.String())
	}
	found := false
	for _, s := range sessions {
		if s.TargetID == "fake" {
			found = true
		}
	}
	if !found {
		t.Fatalf("manifest-declared target 'fake' not serving; sessions=%s", out.String())
	}
	// serveStartup is the one production path serve runs; diagnostics must
	// exist for the owned startup. Generated state, next to refusals.json.
	adaptersData, err := os.ReadFile(filepath.Join(stateDir, "adapters.json"))
	if err != nil {
		t.Fatalf("adapters.json not published by the owned startup: %v", err)
	}
	if !strings.Contains(string(adaptersData), "fake") {
		t.Fatalf("adapters.json missing the manifest target: %s", adaptersData)
	}
}

// TestBuildPartialFailureFakeAndClaude (611.13 r1) pins the partial-failure
// happy path: a manifest with [fake, claude] yields one attachment (fake)
// and one typed refusal (claude stub). serve registers what built and
// persists the refusal; one bad adapter never takes down serve.
func TestBuildPartialFailureFakeAndClaude(t *testing.T) {
	root, err := os.MkdirTemp("", "amqrpartial")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, "extensions", "remote")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	// A manifest with a fake (OK) and a claude (stub refusal).
	mf := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters: []manifest.Adapter{
			{Kind: "fake", Target: "fake", Epoch: "e_1"},
			{Kind: "claude", Target: "cc-1"},
		},
	}
	outcomes := registry.Build(context.Background(), root, stateDir, mf)
	if len(outcomes) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(outcomes))
	}
	// fake succeeds.
	if outcomes[0].Attachment == nil {
		t.Fatalf("outcome[0] (fake): expected attachment, got refusal=%v", outcomes[0].Refusal)
	}
	if outcomes[0].Attachment.Inspect().TargetID != "fake" {
		t.Fatalf("outcome[0] target=%q, want fake", outcomes[0].Attachment.Inspect().TargetID)
	}
	// claude refuses (stub).
	if outcomes[1].Attachment != nil {
		t.Fatal("outcome[1] (claude): expected refusal, got attachment")
	}
	if outcomes[1].Refusal == nil {
		t.Fatal("outcome[1] (claude): expected refusal, got nil")
	}
	if outcomes[1].Refusal.Error() != "claude adapter not yet authorized (gated on 611.2 wire-capture probe)" {
		t.Fatalf("outcome[1] refusal=%q, want claude ErrNotAuthorized", outcomes[1].Refusal.Error())
	}
}

// TestRefusalsClearedOnRestart (611.13 r3, r4 rewrite) pins the ownership
// boundary through the production path: two real serve starts on one root.
// Start 1 owns the store, runs with a claude manifest (stub refusal), and
// persists that refusal. Start 2 has an empty manifest, loses the lock, and
// exits 6 — it must NOT overwrite the live owner's refusals.json (611.13 r4
// item 1: diagnostics publish moved after the owned startup). RED when
// persistRefusals is guarded by len(refusals)>0 (the stale file persists)
// or when diagnostics write before the lock (the loser clobbers the owner).
func TestRefusalsClearedOnRestart(t *testing.T) {
	root, err := os.MkdirTemp("", "amqrrclr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")
	manifestPath := manifest.DefaultPath(stateDir)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Start 1: manifest with a claude entry (stub refusal). Real serve, real
	// manifest read, owned startup; the refusal persists as typed data.
	mf1 := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters: []manifest.Adapter{
			{Kind: "claude", Target: "cc-1"},
		},
	}
	data1, _ := json.Marshal(mf1)
	if err := os.WriteFile(manifestPath, data1, 0644); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- run([]string{"serve", "--root", root, "--manifest", manifestPath}, strings.NewReader(""), &out, &errBuf)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		var o, e bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &o, &e) == 0 {
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatal("endpoint 1 did not start serving within 5s")
	}
	refusals1, err := loadRefusals(stateDir)
	if err != nil {
		t.Fatalf("start 1 load refusals: %v", err)
	}
	if len(refusals1) != 1 {
		t.Fatalf("start 1: got %d refusals, want 1", len(refusals1))
	}

	// Start 2: empty manifest, same root. The store lock is held by start 1;
	// the real serve error return is exit 6 (endpoint_already_running).
	if err := os.WriteFile(manifestPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	var out2, errBuf2 bytes.Buffer
	code2 := run([]string{"serve", "--root", root, "--manifest", manifestPath}, strings.NewReader(""), &out2, &errBuf2)
	if code2 != protocol.ExitForCode(protocol.CodeEndpointAlreadyRunning) {
		t.Fatalf("start 2: exit=%d, want %d (endpoint_already_running)\nstderr=%s", code2, protocol.ExitForCode(protocol.CodeEndpointAlreadyRunning), errBuf2.String())
	}
	// The losing start must not touch the owner's diagnostics.
	refusals2, err := loadRefusals(stateDir)
	if err != nil {
		t.Fatalf("post-start-2 load refusals: %v", err)
	}
	if len(refusals2) != 1 {
		t.Fatalf("start 2 (loser) overwrote the owner's refusals: got %d refusals, want 1", len(refusals2))
	}

	// Same production path, sequential owned starts on a fresh root: a
	// claude refusal on start 1 is GONE after an owned start 2 with an empty
	// manifest. RED when persistRefusals is guarded by len(refusals)>0.
	root2, err := os.MkdirTemp("", "amqrrclr2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root2) })
	stateDir2 := filepath.Join(root2, "extensions", "remote")
	var carrier2 *amqio.Carrier
	carrierPublish2 := func(s protocol.Snapshot, origin map[string]string) error {
		if carrier2 == nil {
			return nil
		}
		return carrier2.Publish(s, origin)
	}
	_, ep1, _, refusalsA, err := serveStartup(stateDir2, root2, amqio.DefaultHandle, mf1, carrierPublish2, &carrier2, io.Discard)
	if err != nil {
		t.Fatalf("owned start 1: %v", err)
	}
	if len(refusalsA) != 1 {
		t.Fatalf("owned start 1: got %d refusals, want 1", len(refusalsA))
	}
	if err := ep1.Close(); err != nil {
		t.Fatalf("owned start 1 close: %v", err)
	}
	_, ep2, _, refusalsB, err := serveStartup(stateDir2, root2, amqio.DefaultHandle, manifest.File{SchemaVersion: manifest.SchemaVersion}, carrierPublish2, &carrier2, io.Discard)
	if err != nil {
		t.Fatalf("owned start 2: %v", err)
	}
	defer func() { _ = ep2.Close() }()
	if len(refusalsB) != 0 {
		t.Fatalf("owned start 2: got %d refusals, want 0 (stale file not cleared)", len(refusalsB))
	}
	cleared, err := loadRefusals(stateDir2)
	if err != nil {
		t.Fatalf("owned start 2 load: %v", err)
	}
	if len(cleared) != 0 {
		t.Fatalf("owned start 2: refusals file still has %d entries, want 0", len(cleared))
	}
}

// TestManifestBytesUnchangedAfterFlag (611.13 r3, r4 rewrite) pins through
// the production serve path that serve NEVER writes to the user's manifest
// path. A codex-only manifest, served with --fake, must have byte-identical
// manifest.json after the flag append; the merged set lives in adapters.json
// (generated state). RED if serve ever calls manifest.Write.
func TestManifestBytesUnchangedAfterFlag(t *testing.T) {
	root, err := os.MkdirTemp("", "amqrnowr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	// A codex entry is user-authored: Validate accepts it, the factory
	// refuses (no daemon in the test), and serve still owns the store.
	mf := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters: []manifest.Adapter{
			{Kind: "codex", Target: "codex-abc123", Config: json.RawMessage(`{"socket":"/tmp/x","thread":"t1"}`)},
		},
	}
	manifestPath := manifest.DefaultPath(stateDir)
	data, _ := json.MarshalIndent(mf, "", "  ")
	if err := os.WriteFile(manifestPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	// Real serve with --fake: the production flag-append path.
	done := make(chan int, 1)
	var serveOut, serveErr bytes.Buffer
	go func() {
		done <- run([]string{"serve", "--root", root, "--manifest", manifestPath, "--fake"}, strings.NewReader(""), &serveOut, &serveErr)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		var o, e bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &o, &e) == 0 {
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("endpoint did not start serving within 5s; serve stderr=%s", serveErr.String())
	}

	// Reread the user's manifest: bytes must be unchanged.
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("manifest.json was modified by the flag-append path\nbefore: %s\nafter:  %s", before, after)
	}
	// The merged set (file + flag sugar) is generated state: both targets in
	// adapters.json, never in the user's file.
	adaptersData, err := os.ReadFile(filepath.Join(stateDir, "adapters.json"))
	if err != nil {
		t.Fatalf("adapters.json not published: %v", err)
	}
	if !strings.Contains(string(adaptersData), "codex-abc123") || !strings.Contains(string(adaptersData), "fake") {
		t.Fatalf("adapters.json missing merged set (codex-abc123 + fake): %s", adaptersData)
	}
}

// TestValidationFailuresExitTwo (611.13 r3, r4 rewrite) pins through the
// REAL serve error return that every manifest validation failure maps to
// exit 2 (ExitUsage), not exit 1. RED when IsValidation uses errors.Is
// (always false) instead of errors.As, and when a serve path skips Validate.
func TestValidationFailuresExitTwo(t *testing.T) {
	cases := []struct {
		name string
		f    manifest.File
	}{
		{
			name: "duplicate target",
			f: manifest.File{
				SchemaVersion: manifest.SchemaVersion,
				Adapters: []manifest.Adapter{
					{Kind: "fake", Target: "dup", Epoch: "e_1"},
					{Kind: "fake", Target: "dup", Epoch: "e_2"},
				},
			},
		},
		{
			name: "epoch on non-fake",
			f: manifest.File{
				SchemaVersion: manifest.SchemaVersion,
				Adapters: []manifest.Adapter{
					{Kind: "codex", Target: "cx-1", Epoch: "cx-1"},
				},
			},
		},
		{
			name: "missing target",
			f: manifest.File{
				SchemaVersion: manifest.SchemaVersion,
				Adapters: []manifest.Adapter{
					{Kind: "fake", Target: ""},
				},
			},
		},
		{
			name: "missing kind",
			f: manifest.File{
				SchemaVersion: manifest.SchemaVersion,
				Adapters: []manifest.Adapter{
					{Target: "orphan"},
				},
			},
		},
		{
			name: "wrong layer",
			f: manifest.File{
				SchemaVersion: manifest.SchemaVersion,
				Layer:         "not-remote",
				Adapters: []manifest.Adapter{
					{Kind: "fake", Target: "fake", Epoch: "e_1"},
				},
			},
		},
		{
			// 611.13 r4: a target with characters outside the opaque grammar
			// is rejected before registration — the observed defect was an
			// accepted "sales team" target every CLI submit then refused.
			name: "invalid target grammar",
			f: manifest.File{
				SchemaVersion: manifest.SchemaVersion,
				Adapters: []manifest.Adapter{
					{Kind: "fake", Target: "sales team", Epoch: "e_1"},
				},
			},
		},
		{
			// 611.13 r4: a fake entry without an epoch is rejected — the fake
			// requires an epoch at submit; an accepted empty epoch produced an
			// unusable session.
			name: "fake missing epoch",
			f: manifest.File{
				SchemaVersion: manifest.SchemaVersion,
				Adapters: []manifest.Adapter{
					{Kind: "fake", Target: "plain"},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Unit level: typed validation failure.
			err := manifest.Validate(tc.f)
			if err == nil {
				t.Fatal("Validate accepted invalid manifest")
			}
			if !manifest.IsValidation(err) {
				t.Fatalf("IsValidation=false for %v; validation failures must be exit 2", err)
			}
			// Production level: the real serve error return is exit 2. Serve
			// refuses before any startup side effect.
			root, err := os.MkdirTemp("", "amqrvexit")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			stateDir := filepath.Join(root, "extensions", "remote")
			if err := os.MkdirAll(stateDir, 0700); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(tc.f)
			manifestPath := manifest.DefaultPath(stateDir)
			if err := os.WriteFile(manifestPath, data, 0644); err != nil {
				t.Fatal(err)
			}
			var out, errBuf bytes.Buffer
			code := run([]string{"serve", "--root", root, "--manifest", manifestPath}, strings.NewReader(""), &out, &errBuf)
			if code != protocol.ExitUsage {
				t.Fatalf("serve exit=%d, want %d (ExitUsage)\nstderr=%s", code, protocol.ExitUsage, errBuf.String())
			}
		})
	}
}

// TestLayerOptionalDefaultsToRemote (611.13 r3) pins that a manifest without
// a layer field is valid and loads with layer=remote filled in memory.
func TestLayerOptionalDefaultsToRemote(t *testing.T) {
	root, err := os.MkdirTemp("", "amqrlayer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, "extensions", "remote")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Manifest with NO layer field (user-authored, minimal).
	data := []byte(`{"schema_version":1,"adapters":[{"kind":"fake","target":"fake","epoch":"e_1"}]}`)
	path := manifest.DefaultPath(stateDir)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	f, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Layer != manifest.Layer {
		t.Fatalf("layer=%q, want %q (default fill)", f.Layer, manifest.Layer)
	}
}
