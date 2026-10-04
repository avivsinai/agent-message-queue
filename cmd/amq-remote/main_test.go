package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// weakFakeFactory registers a test-local registry kind "weak-fake" backed by
// the fake Runtime with its evidence projection weakened to submit=submitted
// (weaker than the fake's default admitted). This exercises the endpoint's
// MinEvidence floor refusal end-to-end WITHOUT adding a shipped config
// setting to the fake: the seam is the existing registry, used only from
// this test file.
//
// recordingFactory registers "recording-fake": the fake Runtime wrapped so
// it records whether the submit's spool envelope was durable when the
// endpoint dispatched it (TestCLIB7PersistBeforeDispatchViaSubmit).
func init() {
	registry.Register("weak-fake", func(ctx context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
		r := fake.New(cfg.Target, "e_1")
		r.WithEvidence(&protocol.Evidence{Submit: "submitted", Completion: "run_terminal"})
		return r, nil
	})
	registry.Register("recording-fake", func(ctx context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
		rf := &recordingFake{Runtime: fake.New(cfg.Target, "e_1"), stateDir: cfg.StateDir}
		lastRecordingFake.Store(rf)
		return rf, nil
	})
}

// startServeWithArgs starts serve with explicit arguments and the same
// readiness probe + cleanup contract as startServe.
func startServeWithArgs(t *testing.T, args []string, root string) {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- run(args, strings.NewReader(""), &out, &errBuf)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var probeOut, probeErr bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &probeOut, &probeErr) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("endpoint did not start serving within 5s")
}

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
	// The achieved evidence class rides on the reply (architect review): the
	// fake proves admitted. The human evidence= line is pinned by
	// TestCLISubmitMinEvidenceUnsupportedExits6.
	if rep.Outcome.Evidence != protocol.EvidenceAdmitted {
		t.Fatalf("Outcome.Evidence=%q, want %q", rep.Outcome.Evidence, protocol.EvidenceAdmitted)
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
// reaches a harness: it is a usage error at the CLI boundary, refused before
// the endpoint is contacted.
func TestCLISubmitRejectsEmptyPrompt(t *testing.T) {
	code, _, _ := cli(t, "", "submit", "fake", "--text", "   ", "--root", t.TempDir(), "--json")
	if code != protocol.ExitUsage {
		t.Fatalf("whitespace-only prompt exit = %d, want %d (usage)", code, protocol.ExitUsage)
	}
}

// TestReplyRouterForTransientPeerAbsent exercises the SHIPPED adapter
// (replyRouterFor), not a reimplemented copy. While the caller's root (B1) or
// the caller's session under an existing root (agent-message-queue-611.22.36
// packet 7) does not exist yet, the adapter must wrap the router's
// ErrPeerRootUnreachable in amqio.TransientRouteError so the carrier leaves
// the command in new (transient) instead of DLQ'ing it (poison). Once the
// destination exists, the next import delivers the reply into it. Deleting
// the translation from replyRouterFor must fail this test.
func TestReplyRouterForTransientPeerAbsent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseExists bool
	}{
		{"peer root absent", false},
		{"peer session absent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			amqrcData, _ := json.Marshal(map[string]any{
				"project": "endpoint",
				"root":    ".agent-mail",
				"peers":   map[string]string{"caller": filepath.Join("..", "caller", ".agent-mail")},
			})
			if err := os.WriteFile(filepath.Join(endpointBase, ".amqrc"), amqrcData, 0o644); err != nil {
				t.Fatalf("write .amqrc: %v", err)
			}
			callerRoot := filepath.Join(filepath.Dir(endpointBase), "caller", ".agent-mail")
			if tc.baseExists {
				if err := fsq.EnsureRootDirs(callerRoot); err != nil {
					t.Fatal(err)
				}
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
			// Use the SHIPPED adapter — not a reimplemented closure.
			carrier.SetReplyRouter(replyRouterFor(endpointRoot))
			ep.Register(fake.New("fake", "e_1"))
			t.Cleanup(func() { _ = ep.Close() })

			identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
			droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
			defer func() { _ = droot.Close() }()
			now := time.Now()
			id, _ := format.NewMessageID(now)
			body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111381","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(now.Add(time.Minute)) + `","input":{"text":"hi"}}`
			msg := format.Message{Header: format.Header{
				Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{amqio.DefaultHandle},
				Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
				FromProject: "caller", ReplyTo: "codex@collab", ReplyProject: "caller",
			}, Body: body}
			data, _ := msg.Marshal()
			if _, err := fsq.DeliverToInboxes(droot, []string{amqio.DefaultHandle}, id+".md", data); err != nil {
				t.Fatalf("deliver: %v", err)
			}

			// TICK 1: the destination does not exist. Transient: the command
			// stays in new and is not dead-lettered.
			if n, _ := carrier.ImportOnce(); n != 0 {
				t.Fatalf("TICK1: command handled (%d) although the destination does not exist (must be transient)", n)
			}
			if entries, _ := os.ReadDir(fsq.AgentInboxNew(endpointRoot, amqio.DefaultHandle)); len(entries) != 1 {
				t.Fatalf("TICK1: command should stay in new, found %d", len(entries))
			}
			if entries, _ := os.ReadDir(filepath.Join(endpointRoot, "agents", amqio.DefaultHandle, "dlq", "new")); len(entries) != 0 {
				t.Fatalf("TICK1: transient command was dead-lettered (an absent destination is not poison): %d", len(entries))
			}

			// The caller's root, session and codex mailbox appear.
			sessionRoot := filepath.Join(callerRoot, "collab")
			for _, r := range []string{callerRoot, sessionRoot} {
				if err := fsq.EnsureRootDirs(r); err != nil {
					t.Fatal(err)
				}
			}
			if err := fsq.EnsureAgentDirs(sessionRoot, "codex"); err != nil {
				t.Fatal(err)
			}

			// TICK 2: the adapter resolves, the carrier delivers the reply,
			// and the command is claimed.
			n, err := carrier.ImportOnce()
			if err != nil || n != 1 {
				t.Fatalf("TICK2: n=%d err=%v, want the command handled", n, err)
			}
			if entries, _ := os.ReadDir(fsq.AgentInboxCur(endpointRoot, amqio.DefaultHandle)); len(entries) != 1 {
				t.Fatalf("TICK2: command not claimed into cur: %d", len(entries))
			}
			if entries, _ := os.ReadDir(fsq.AgentInboxNew(sessionRoot, "codex")); len(entries) == 0 {
				t.Fatal("TICK2: no reply in the caller session's codex inbox")
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

// TestSenderB7RefusedNotDispatched is the B2 regression at the submit
// boundary: the endpoint returns a refusal (stale_epoch) as a Reply with
// Outcome.Code and a nil error, and submit must mark its envelope failed with
// that code, never dispatched, and exit 6.
func TestSenderB7RefusedNotDispatched(t *testing.T) {
	root := startServe(t)
	stateDir := filepath.Join(root, "extensions", "remote")
	const id = "11111111-1111-4111-8111-1111111117b2"
	code, out, _ := cli(t, "", "submit", "fake", "--text", "will be refused",
		"--root", root, "--epoch", "e_stale", "--request-id", id, "--json")
	if code != protocol.ExitActionRequired {
		t.Fatalf("stale-epoch submit exit=%d, want %d out=%s", code, protocol.ExitActionRequired, out)
	}
	spool, err := sender.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	env, exists, err := spool.Get(ipc.LocalHost, id)
	if err != nil || !exists {
		t.Fatalf("envelope missing after a refused submit: exists=%v err=%v", exists, err)
	}
	if env.State != sender.StateFailed || env.LastError != string(protocol.CodeStaleEpoch) {
		t.Fatalf("refused submit left envelope state=%s last_error=%q, want failed/stale_epoch (B2)", env.State, env.LastError)
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer (review ruling 20:43:46Z): the
// serve tick loop writes diagnostics while the failure report reads them, and
// a raw bytes.Buffer shared between goroutines is a data race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{} }

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServeCaptured is startServe with the serve goroutine's stdout/stderr
// retained (review ruling 19:53Z: the reap diagnosis needs the tick loop's
// own reports - drain/import/tick errors - at failure time). Test-local to
// this regression; other tests keep startServe.
func startServeCaptured(t *testing.T) (string, *syncBuffer, *syncBuffer) {
	t.Helper()
	root, err := os.MkdirTemp("", "amqr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	out, errBuf := newSyncBuffer(), newSyncBuffer()
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"serve", "--fake", "--root", root}, strings.NewReader(""), out, errBuf)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var probeOut, probeErr bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &probeOut, &probeErr) == 0 {
			return root, out, errBuf
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("endpoint did not start serving within 5s")
	return "", nil, nil
}

// TestSenderB7ReapWithoutDrain is the B4 regression: Reap runs on every
// serve tick, independent of Drain activity.
func TestSenderB7ReapWithoutDrain(t *testing.T) {
	root, serveOut, serveErr := startServeCaptured(t)
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
	// Read-back the aged write immediately (review ruling 19:53Z): if the
	// on-disk SettledAt is not aged here, the write raced a serve-side
	// rewrite - that is a different failure than Reap not running.
	back, err := os.ReadFile(envPath)
	if errors.Is(err, os.ErrNotExist) {
		// Review ruling 20:43:46Z: a successful Reap may remove the file
		// between aging and this read-back. Absence here IS successful
		// reaping, not a broken diagnostic.
		return
	}
	if err != nil {
		t.Fatalf("aged envelope read-back failed: %v", err)
	}
	var backEnv sender.Envelope
	if err := json.Unmarshal(back, &backEnv); err != nil {
		t.Fatalf("aged envelope read-back is not valid JSON: %v\n%s", err, back)
	}
	if backEnv.SettledAt != env.SettledAt {
		t.Fatalf("aged SettledAt clobbered immediately after write: wrote %q, on disk %q (state=%s)", env.SettledAt, backEnv.SettledAt, backEnv.State)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, exists, _ := spool.Get(ipc.LocalHost, "11111111-1111-4111-8111-1111111117b4")
		if !exists {
			return // reaped!
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Bounded failure evidence (review ruling 19:53Z): final envelope
	// state/SettledAt from disk, state dir listing, and the serve loop's own
	// stderr/stdout (tick/import/drain errors). No retries, no loop.
	after, _ := os.ReadFile(envPath)
	var report strings.Builder
	report.WriteString("envelope was not reaped within 5s (B4: Reap not running on every tick)\n")
	if len(after) > 0 {
		var fin sender.Envelope
		if err := json.Unmarshal(after, &fin); err == nil {
			fmt.Fprintf(&report, "final envelope: state=%s settledAt=%q dispatchedAt=%q lastError=%q\n", fin.State, fin.SettledAt, fin.DispatchedAt, fin.LastError)
		} else {
			fmt.Fprintf(&report, "final envelope unreadable: %v\n", err)
		}
	} else {
		report.WriteString("final envelope: file absent at failure time\n")
	}
	entries, _ := os.ReadDir(stateDir)
	report.WriteString("state dir:\n")
	for _, entry := range entries {
		fmt.Fprintf(&report, "  %s\n", entry.Name())
	}
	fmt.Fprintf(&report, "serve stderr:\n%s\n", serveErr.String())
	fmt.Fprintf(&report, "serve stdout:\n%s\n", serveOut.String())
	t.Fatalf("%s", report.String())
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

// recordingFake wraps fake.Runtime and records whether the submit's spool
// envelope existed at the moment the endpoint dispatched it (Submit).
type recordingFake struct {
	*fake.Runtime
	stateDir     string
	envelopeSeen atomic.Bool
}

// lastRecordingFake is the instance the "recording-fake" factory built.
var lastRecordingFake atomic.Pointer[recordingFake]

func (r *recordingFake) Submit(req core.BoundRequest) (core.Admission, error) {
	if spool, err := sender.Open(r.stateDir); err == nil {
		if _, ok, _ := spool.Get(req.Key.CreatorHost, req.Key.RequestID); ok {
			r.envelopeSeen.Store(true)
		}
	}
	return r.Runtime.Submit(req)
}

// TestCLIB7PersistBeforeDispatchViaSubmit (round-4) drives the REAL CLI
// submit path against a real serve and proves the envelope is durable BEFORE
// the endpoint dispatches the request. RED when submit's spool Create moves
// after the live dispatch, or is deleted.
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
	manifestPath := manifest.DefaultPath(stateDir)
	if err := manifest.Write(manifestPath, manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters:      []manifest.Adapter{{Kind: "recording-fake", Target: "rec"}},
	}); err != nil {
		t.Fatal(err)
	}
	startServeWithArgs(t, []string{"serve", "--root", root}, root)
	rf := lastRecordingFake.Load()
	if rf == nil || rf.stateDir != stateDir {
		t.Fatal("serve did not build the recording-fake adapter from the manifest")
	}

	code, out, _ := cli(t, "", "submit", "rec", "--root", root,
		"--text", "b7 probe", "--request-id", "11111111-1111-4111-8111-11111111b701", "--epoch", "e_1")
	if code != 0 {
		t.Fatalf("submit exit=%d out=%s", code, out)
	}
	if !rf.envelopeSeen.Load() {
		t.Fatal("B7: the spool envelope did NOT exist when the endpoint dispatched the submit (persist-after-dispatch or deleted Create)")
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
	store, ep, err := openServeStore(stateDir)
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

// TestCLISubmitMinEvidenceUnsupportedExits6 drives an ACTUAL unsupported
// outcome through the CLI (bead ccw: the landed version was a tautology that
// never produced an unsupported outcome). A manifest-configured fake adapter
// simulates a weaker evidence projection (submit=submitted); a submit with
// --min-evidence admitted is then refused CodeUnsupported by the endpoint's
// floor check and must map to exit 6 (action-required / capability mismatch),
// NOT exit 1 (failure) - the caller must not take the "work failed" recovery
// path for a capability mismatch.
func TestCLISubmitMinEvidenceUnsupportedExits6(t *testing.T) {
	root, err := os.MkdirTemp("", "amqrex6")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	// Weak fake: manifest config overrides the simulated evidence to
	// submit=submitted (weaker than admitted). The manifest must exist
	// BEFORE serve starts - the registry is constructed at startup.
	stateDir := filepath.Join(root, "extensions", "remote")
	manifestPath := manifest.DefaultPath(stateDir)
	mf := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters: []manifest.Adapter{{
			Kind:   "weak-fake",
			Target: "weak-fake",
		}},
	}
	data, mErr := json.Marshal(mf)
	if mErr != nil {
		t.Fatal(mErr)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	startServeWithArgs(t, []string{"serve", "--root", root, "--manifest", manifestPath}, root)

	// The floor the weak fake cannot meet: requiring admitted is refused
	// before any dispatch, and the CLI maps it to exit 6.
	code, out, errOut := cli(t, "", "submit", "weak-fake", "--text", "floored", "--min-evidence", "admitted", "--root", root)
	if code != protocol.ExitActionRequired {
		t.Fatalf("unmet floor submit exit=%d, want %d (ExitActionRequired)\nout=%s\nerr=%s", code, protocol.ExitActionRequired, out, errOut)
	}
	// The refusal names the capability mismatch, not a generic failure.
	if !strings.Contains(out+errOut, "unsupported") && !strings.Contains(out+errOut, "min_evidence") && !strings.Contains(out+errOut, "evidence") {
		t.Fatalf("exit-6 refusal does not name the evidence mismatch:\nout=%s\nerr=%s", out, errOut)
	}

	// Control: the same weak fake meets the submitted floor - exits 0 and
	// prints the weaker evidence class actually proven.
	code, out, errOut = cli(t, "", "submit", "weak-fake", "--text", "floored", "--min-evidence", "submitted", "--root", root)
	if code != 0 {
		t.Fatalf("met floor submit exit=%d, want 0 (out=%s err=%s)", code, out, errOut)
	}
	if !strings.Contains(out, "evidence=submitted") {
		t.Fatalf("met-floor submit missing evidence=submitted:\n%s", out)
	}
}

// TestBK4P0RunningStaysRunningAfterReconcile pins the order startupSequence
// runs (open store, SetPublish, carrier, wire, Register, then Reconcile) on
// a running record left by a restart mid-run. Each inversion is a defect
// that was observed:
//   - Reconcile before Register (round-5 P0): the record is marked
//     uncertain/attachment_lost although its run is alive.
//   - Reconcile before SetPublish (round-5 P0): the first revision is lost
//     to a no-op publisher.
//   - carrier built after Reconcile (round-6): serve's publish closure sees
//     no carrier during the startup revision.
//   - carrier wiring after Reconcile (w4x, review-a13): the startup revision
//     publishes through an unwired carrier.
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
	seedStore, err := requests.Open(stateDir, requests.WithMaxStoreBytes(protocol.DefaultMaxStoreBytes))
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

	// The publish callback mirrors serve's carrierPublish closure: it
	// forwards to the carrier variable startupSequence assigns.
	var mu sync.Mutex
	var carrier *amqio.Carrier
	var wired, published, carrierMissing, unwired bool
	var publishedRev int64
	wire := func(*amqio.Carrier) {
		mu.Lock()
		defer mu.Unlock()
		wired = true
	}
	publish := func(s protocol.Snapshot, origin map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		published, publishedRev = true, s.Revision
		carrierMissing = carrierMissing || carrier == nil
		unwired = unwired || !wired
		return nil
	}
	store, ep, _, err := startupSequence(stateDir, root, amqio.DefaultHandle, publish, &carrier, wire, rt)
	if err != nil {
		t.Fatalf("startupSequence: %v", err)
	}
	defer func() { _ = ep.Close() }()

	got, exists, err := store.Get(k)
	if err != nil || !exists {
		t.Fatalf("record missing: exists=%v err=%v", exists, err)
	}
	if got.State != protocol.StateRunning {
		t.Fatalf("running record state=%s code=%s after startup, want running (Reconcile ran before Register)", got.State, got.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if !published || publishedRev < 2 {
		t.Fatalf("startup revision never reached the publisher (published=%v rev=%d): Reconcile ran before SetPublish", published, publishedRev)
	}
	if carrierMissing {
		t.Fatal("carrier was nil when Reconcile published: carrier constructed after Reconcile")
	}
	if unwired {
		t.Fatal("Reconcile published before the carrier was wired (w4x)")
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

	// Start 1: manifest with a claude entry (stub refusal). Bounded owned
	// startup via serveStartupFrom (the ONE production path serve runs) + the
	// bounded IPC server; both are Closed so the lock/listener never outlive
	// the test (611.13 r5).
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
	ownedStartup(t, stateDir, root, manifestPath, nil)
	refusals1, err := loadRefusals(stateDir)
	if err != nil {
		t.Fatalf("start 1 load refusals: %v", err)
	}
	// The persisted refusal is what doctor prints: the adapter, and the
	// factory's own reason (611.13 r1: one bad adapter is a typed refusal,
	// not a lost start).
	if len(refusals1) != 1 || refusals1[0].Kind != "claude" || refusals1[0].Target != "cc-1" || !strings.Contains(refusals1[0].Error, "pid is required") {
		t.Fatalf("start 1: refusals = %+v, want one claude cc-1 refusal naming the missing pid", refusals1)
	}
	// The owner must be reachable: the real serve path owns the lock while
	// the losing start-2 runs against it.
	resp, err := ipc.Call(stateDir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionList}})
	if err != nil {
		t.Fatalf("owner not reachable over ipc: %v", err)
	}
	_ = resp

	// Start 2: empty manifest, same root. The store lock is held by start 1;
	// the real serve error return is exit 6 (endpoint_already_running). This
	// is the one real-CLI invocation in the test: a full run(serve) against
	// the LIVE owner, the exact observed defect boundary (611.13 r4 item 1).
	emptyPath := filepath.Join(stateDir, "manifest-empty.json")
	if err := os.WriteFile(emptyPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	var out2, errBuf2 bytes.Buffer
	code2 := run([]string{"serve", "--root", root, "--manifest", emptyPath}, strings.NewReader(""), &out2, &errBuf2)
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
	// Sequential owned starts through the shared production path, fresh
	// roots, nothing perpetual: each endpoint is Closed before the next
	// start, releasing the lock (611.13 r5 lifecycle).
	ep1, refusalsA, err := startFromManifest(stateDir2, root2, manifestPath, nil)
	if err != nil {
		t.Fatalf("owned start 1: %v", err)
	}
	if len(refusalsA) != 1 {
		t.Fatalf("owned start 1: got %d refusals, want 1", len(refusalsA))
	}
	if err := ep1.Close(); err != nil {
		t.Fatalf("owned start 1 close: %v", err)
	}
	emptyFile := filepath.Join(stateDir2, "manifest-empty.json")
	if err := os.WriteFile(emptyFile, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	ep2, refusalsB, err := startFromManifest(stateDir2, root2, emptyFile, nil)
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

// startFromManifest loads manifestFile the way serve does and runs the ONE
// production startup path (serveStartupFrom: validate -> Build -> owned
// sequence -> diagnostics publish) with the shipped carrier wiring.
func startFromManifest(stateDir, root, manifestFile string, sugar []manifest.Adapter) (*core.Endpoint, []registry.Outcome, error) {
	mf, err := manifest.Load(manifestFile)
	if err != nil {
		return nil, nil, err
	}
	noop := func(protocol.Snapshot, map[string]string) error { return nil }
	_, ep, _, refusals, err := serveStartupFrom(stateDir, root, amqio.DefaultHandle, mf, sugar, noop, nil, io.Discard, wireCarrier(root, io.Discard))
	return ep, refusals, err
}

// ownedStartup runs startFromManifest plus the bounded IPC server, and
// registers t.Cleanup that closes both EVEN ON assertion failure. Nothing
// perpetual: the owner lock and listener are released before RemoveAll
// (611.13 r5 lifecycle rule).
func ownedStartup(t *testing.T, stateDir, root, manifestFile string, sugar []manifest.Adapter) {
	t.Helper()
	ep, _, err := startFromManifest(stateDir, root, manifestFile, sugar)
	if err != nil {
		t.Fatalf("owned startup: %v", err)
	}
	server, err := ipc.Listen(stateDir, ep)
	if err != nil {
		_ = ep.Close()
		t.Fatalf("ipc listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		_ = server.Serve(ctx)
		close(served)
	}()
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
		_ = ep.Close()
		<-served
	})
}

// TestManifestBytesUnchangedAfterFlag (611.13 r3, r4/r5 rewrite) pins through
// the production startup path that serve NEVER writes to the user's manifest
// path. A codex-only manifest, started with --fake sugar, must have
// byte-identical manifest.json after the flag append; the merged set lives in
// adapters.json (generated state). RED if the production path ever calls
// manifest.Write.
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
	// refuses (guaranteed-absent socket inside THIS test's root), and the
	// owned startup persists anyway.
	absentSocket := filepath.Join(root, "absent.sock")
	mf := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters: []manifest.Adapter{
			{Kind: "codex", Target: "codex-abc123", Config: json.RawMessage(`{"socket":"` + absentSocket + `","thread":"t1"}`)},
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

	// Bounded production startup with --fake sugar (the flag-append path).
	ownedStartup(t, stateDir, root, manifestPath, []manifest.Adapter{{Kind: "fake", Target: "fake", Epoch: "e_1"}})

	// Reread the user's manifest: bytes must be unchanged.
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("manifest.json was modified by the flag-append path\nbefore: %s\nafter:  %s", before, after)
	}
	// The merged set (file + flag sugar) is generated state: both targets in
	// adapters.json, never in the user's file. An attached entry advertises
	// its manifest kind (review-824-r1 P1-2: an empty "kind" was the
	// review-b7 defect).
	adaptersData, err := os.ReadFile(filepath.Join(stateDir, "adapters.json"))
	if err != nil {
		t.Fatalf("adapters.json not published: %v", err)
	}
	var effective []struct{ Kind, Target, State string }
	if err := json.Unmarshal(adaptersData, &effective); err != nil {
		t.Fatalf("adapters.json: %v", err)
	}
	got := map[string]string{}
	for _, e := range effective {
		got[e.Target] = e.Kind + "/" + e.State
	}
	if got["fake"] != "fake/attached" || got["codex-abc123"] != "codex/refused" || len(got) != 2 {
		t.Fatalf("adapters.json = %s, want fake attached (kind fake) and codex-abc123 refused", adaptersData)
	}
}

// TestValidationFailuresExitTwo (611.13 r3, r4 rewrite) pins through the
// REAL serve error return that every manifest validation failure maps to
// exit 2 (ExitUsage), not exit 1: one row per validation error type, since
// each type opts into IsValidation on its own. RED when IsValidation uses
// errors.Is (always false) instead of errors.As, and when a serve path skips
// Validate.
func TestValidationFailuresExitTwo(t *testing.T) {
	cases := []struct {
		name     string
		f        manifest.File
		discover bool
		// wantErr (review-824-r1 P1-3) pins the diagnostic wording on the
		// manifest-internal duplicate case: the blame must name the
		// MANIFEST, not the flags.
		wantErr string
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
			wantErr: "manifest declares the same target id more than once",
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
		{
			// 611.13 r5 regression observed by codex review: --discover must
			// classify a validation failure exactly like serve — the same
			// invalid manifest exited 1 under --discover and 2 under serve.
			name: "discover invalid target exits two",
			f: manifest.File{
				SchemaVersion: manifest.SchemaVersion,
				Adapters: []manifest.Adapter{
					{Kind: "fake", Target: "sales team", Epoch: "e_1"},
				},
			},
			discover: true,
		},
	}
	// review-824-r1 P2-3: ONE deadline for the whole table. Serve refuses
	// in well under a second for a correct build; a regression that leaves
	// serve running must fail the table fast, not burn 8x30s of CI.
	tableDeadline := time.Now().Add(30 * time.Second)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The real serve error return is exit 2. Serve refuses before
			// any startup side effect.
			root := t.TempDir()
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
			var code int
			// 611.13.4-3: bound the test wait on the in-process serve. For a
			// VALID manifest a mutation could leave serve running forever
			// and stall the whole package for the full go-test timeout.
			// Serve must refuse an invalid manifest before startup; 30s is
			// generous even under heavy CI load. The deadline bounds this
			// test's wait, NOT the serve goroutine (which keeps running in
			// the mutation case and still owns out/errBuf).
			type serveResult struct{ code int }
			done := make(chan serveResult, 1)
			go func() {
				if tc.discover {
					code := run([]string{"serve", "--discover", "--root", root, "--manifest", manifestPath}, strings.NewReader(""), &out, &errBuf)
					done <- serveResult{code}
					return
				}
				code := run([]string{"serve", "--root", root, "--manifest", manifestPath}, strings.NewReader(""), &out, &errBuf)
				done <- serveResult{code}
			}()
			select {
			case r := <-done:
				code = r.code
			case <-time.After(time.Until(tableDeadline)):
				// Review ruling 20:56:30Z: the serve goroutine still owns
				// out/errBuf, so reading them here races with its writes
				// (the same failure-path race fixed in #820). Report
				// without touching the live buffers. The deadline is shared
				// across the table (review-824-r1 P2-3): a validation
				// regression costs one bound, not one per subtest.
				t.Fatalf("serve did not refuse the invalid manifest within the shared 30s table deadline (mutation bound; it must exit before any startup side effect); serve goroutine still running and owns the capture buffers")
			}
			if code != protocol.ExitUsage {
				t.Fatalf("serve exit=%d, want %d (ExitUsage)\nstderr=%s", code, protocol.ExitUsage, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), tc.wantErr) {
				t.Fatalf("stderr %q missing expected diagnostic %q", errBuf.String(), tc.wantErr)
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

// TestDoctorRefusalsErrorSurfaced (review-824-r1 P1-1): a truncated
// refusals.json must surface refusals_error in the doctor report (the
// review-b7 defect was a silent erase and exit 0 with no hint); an absent
// refusals.json must NOT set the key (fresh root before first serve).
func TestDoctorRefusalsErrorSurfaced(t *testing.T) {
	root, err := os.MkdirTemp("", "amqrdoc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, "extensions", "remote")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Absent file: no refusals_error key.
	report, _, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatalf("doctor on fresh state dir: %v", err)
	}
	if _, has := report.(map[string]any)["refusals_error"]; has {
		t.Fatalf("refusals_error set for an absent refusals.json: %v", report)
	}

	// Truncated file: the key appears and carries the load error.
	if err := os.WriteFile(filepath.Join(stateDir, "refusals.json"), []byte(`[
  {
    "kind": "cla`), 0600); err != nil {
		t.Fatal(err)
	}
	report, _, err = doctor([]string{"--root", root})
	if err != nil {
		t.Fatalf("doctor on truncated refusals.json: %v", err)
	}
	refErr, has := report.(map[string]any)["refusals_error"]
	if !has {
		t.Fatalf("refusals_error missing for a truncated refusals.json: %v", report)
	}
	if refErr == "" {
		t.Fatalf("refusals_error empty for a truncated refusals.json")
	}
}
