package main

import (
	"bytes"
	"context"
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
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
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
	// Seed config.json with an existing agent so we can verify preservation.
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

	// Boot serve with --me remote.
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

	// Wait for the socket to accept.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out, errBuf bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &out, &errBuf) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Verify config.json now contains both "codex" (preserved) and "remote" (added).
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

// TestServeRejectsInvalidHandle is the B1 serve-boundary test: a bad --me
// handle must not poison config.json. Serve exits because amqio.New rejects
// the handle, but config.json must NOT be created or modified with the
// invalid handle.
func TestServeRejectsInvalidHandle(t *testing.T) {
	root, err := os.MkdirTemp("", "amqr10b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "meta", "config.json")

	// Boot serve with an invalid --me handle. Serve should exit non-zero
	// because amqio.New rejects the handle.
	var out, errBuf bytes.Buffer
	code := run([]string{"serve", "--fake", "--root", root, "--me", "Bad-Handle"}, strings.NewReader(""), &out, &errBuf)
	if code == 0 {
		t.Fatalf("serve with invalid handle exited 0, want non-zero")
	}

	// config.json must NOT contain the invalid handle.
	if _, err := os.Stat(configPath); err == nil {
		data, _ := os.ReadFile(configPath)
		if strings.Contains(string(data), "Bad-Handle") {
			t.Fatalf("invalid handle poisoned config.json: %s", data)
		}
	}
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
