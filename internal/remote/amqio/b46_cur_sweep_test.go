package amqio

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// makeCurEntry writes a valid cur message file and returns its path. The
// message is a well-formed submit so that, once the source file is readable,
// recoverOne can proceed to receipt/ledger/reply and deliver the pending
// reply.
func makeCurEntry(t *testing.T, root, handle string) (path, id string) {
	t.Helper()
	curDir := filepath.Join(root, "agents", handle, "inbox", "cur")
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		t.Fatalf("mkdir cur: %v", err)
	}
	now := time.Now()
	id, _ = format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:       format.CurrentSchema,
			ID:           id,
			From:         "codex",
			To:           []string{handle},
			Thread:       "p2p/codex__remote",
			Subject:      "submit",
			Created:      now.UTC().Format(time.RFC3339Nano),
			Kind:         "todo",
			ReplyProject: "nonexistent-project",
			ReplyTo:      "codex@nonexistent-project",
		},
		Body: `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111461","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"work"}}`,
	}
	data, _ := msg.Marshal()
	path = filepath.Join(curDir, id+".md")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write cur: %v", err)
	}
	return path, id
}

// TestB46ReplyRouteFailureIsNotSuppressed proves the P1 correction: a
// reply-routing failure is NOT a source-read failure, so it must NOT be
// capped. Three identical reply-route failures must NOT quarantine the entry
// — the pending reply obligation stays eligible every tick until the route
// converges. This is the defect codex identified: the previous broad cap
// suppressed reply-route failures and lost the pending reply.
//
// Mutation RED: revert to capping ALL recoverOne errors (remove the
// errCurSourceReadFailed boundary check in recoverCur) -> the entry is
// quarantined after curFailureCap reply-route failures and the "still
// errors every tick past cap" assertion fails.
func TestB46ReplyRouteFailureIsNotSuppressed(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, DefaultHandle); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	makeCurEntry(t, root, DefaultHandle)

	store, err := requests.Open(filepath.Join(root, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ep := core.New(core.Config{Store: store})
	carrier, err := New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	// A router that always fails — a reply-routing failure, NOT a source-read.
	carrier.SetReplyRouter(func(project, replyTo string) (string, string, error) {
		return "", "", errNoRouteForTest
	})

	// Run well past curFailureCap ticks. The reply-route failure must NEVER be
	// suppressed: ImportOnce must keep reporting it every tick (the pending
	// reply obligation stays eligible).
	for tick := 1; tick <= curFailureCap+2; tick++ {
		if _, err := carrier.ImportOnce(); err == nil {
			t.Fatalf("tick %d: reply-route failure was suppressed (611.22.46 P1: only source-read failures may be capped)", tick)
		}
	}
}

// TestB46LateRepairDownstreamReplyFailureStaysPending walks one cur entry
// through the agent-message-queue-611.22.46 sweep cap:
//   - ticks 1..curFailureCap: its source file is unreadable (EACCES), and
//     each tick reports the read error (a failed read is never "recovered");
//   - the next tick: the entry is capped, skipped, and the sweep is done;
//   - repaired source, failing route: recoverCappedCur re-probes the capped
//     entry (NO-GO 2) and the downstream reply failure keeps it pending
//     (NO-GO 3);
//   - restored route: the reply lands exactly once;
//   - one more tick: success cleared the entry, so recovery and the router
//     do not run again (NO-GO 4).
func TestB46LateRepairDownstreamReplyFailureStaysPending(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions; EACCES cannot be simulated")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows chmod does not enforce read denial; EACCES cannot be simulated")
	}
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, DefaultHandle); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	path, _ := makeCurEntry(t, root, DefaultHandle)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	store, err := requests.Open(filepath.Join(root, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ep := core.New(core.Config{Store: store})
	carrier, err := New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}

	callerRoot := t.TempDir()
	if err := fsq.EnsureRootDirs(callerRoot); err != nil {
		t.Fatalf("EnsureRootDirs caller: %v", err)
	}
	if err := fsq.EnsureAgentDirs(callerRoot, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs caller: %v", err)
	}
	callerNewDir := filepath.Join(callerRoot, "agents", "codex", "inbox", "new")

	// The first route attempt fails (a downstream reply-routing failure);
	// every later one succeeds.
	var routeCalls int32
	carrier.SetReplyRouter(func(project, replyTo string) (string, string, error) {
		if atomic.AddInt32(&routeCalls, 1) == 1 {
			return "", "", errNoRouteForTest
		}
		return callerRoot, "codex", nil
	})

	// Ticks 1..curFailureCap: source unreadable (EACCES). Entry caps.
	for i := 1; i <= curFailureCap; i++ {
		if _, err := carrier.ImportOnce(); err == nil {
			t.Fatalf("tick %d: expected source-read error", i)
		}
	}
	// Tick 4: capped, skipped, curRecovered=true.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick 4: expected capped skip (nil), got: %v", err)
	}
	if !carrier.curRecovered {
		t.Fatal("curRecovered should be true")
	}

	// Repair the source. Now readable, but the route STILL fails (phase 1).
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod restore: %v", err)
	}

	// Tick 5: recoverCappedCur re-probes (readable), runs recoverOne, but the
	// route fails (downstream). The entry MUST stay pending (not dropped).
	if _, err := carrier.ImportOnce(); err == nil {
		t.Fatal("tick 5: expected downstream reply-routing failure, got nil")
	}
	// No reply yet.
	if replies, _ := os.ReadDir(callerNewDir); len(replies) > 0 {
		t.Fatal("tick 5: reply delivered despite route failure")
	}

	// Tick 6: route now restored (phase 2). The entry is still pending, so
	// recoverCappedCur retries and the reply lands.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick 6: expected recovery to succeed after route restore, got: %v", err)
	}
	replies, err := os.ReadDir(callerNewDir)
	if err != nil {
		t.Fatalf("read caller inbox/new: %v", err)
	}
	if len(replies) != 1 {
		t.Fatalf("%d replies after route restore, want 1 (611.22.46 3rd NO-GO: downstream failure must keep the entry pending for the next tick)", len(replies))
	}
	routeAfterSuccess := atomic.LoadInt32(&routeCalls)

	// Tick 7: the entry is no longer pending, so recovery must not run again.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick 7: expected nil, got: %v", err)
	}
	if got := atomic.LoadInt32(&routeCalls); got != routeAfterSuccess {
		t.Fatalf("tick 7: router called again after successful recovery (routeCalls %d -> %d) (611.22.46 4th NO-GO: success must clear pendingRecovery)", routeAfterSuccess, got)
	}
}

var errNoRouteForTest = errRoute("no route (test)")

type errRoute string

func (e errRoute) Error() string { return string(e) }
