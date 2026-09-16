package amqio

import (
	"os"
	"path/filepath"
	"runtime"
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

// TestB46UnreadableSourceStopsResweeping reproduces agent-message-queue-611.22.46
// at the correct boundary: a cur entry whose SOURCE FILE cannot be read
// (EACCES — the file is present on disk but unreadable) keeps curRecovered
// false and forces a full O(cur) rescan every tick. The fix caps only this
// source-read failure class (errCurSourceReadFailed) after curFailureCap
// identical failures; receipt/ledger/reply obligations are NOT suppressed.
//
// Mutation RED: remove the curFailureCapped skip in recoverCur -> the entry
// is re-read every tick and ImportOnce keeps returning its read error past
// the cap, so the "no error after cap" assertion fails.
func TestB46UnreadableSourceStopsResweeping(t *testing.T) {
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
	// Make the source file unreadable so format.ReadMessageFileRoot fails with
	// EACCES — a source-read failure (readFailed == true), the exact boundary
	// the bead names. NOT a reply-route failure.
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
	// A router that WOULD succeed for a real project; irrelevant here because
	// the source read fails before routing is reached.
	carrier.SetReplyRouter(func(project, replyTo string) (string, string, error) {
		return filepath.Dir(root), "codex", nil
	})

	// Ticks 1..curFailureCap: the source read fails each time (EACCES).
	for i := 1; i <= curFailureCap; i++ {
		if _, err := carrier.ImportOnce(); err == nil {
			t.Fatalf("tick %d: expected a source-read error, got nil", i)
		}
	}
	// Tick curFailureCap+1: the entry is quarantined (source-read failure cap
	// reached). recoverOne is NOT called, so ImportOnce no longer reports the
	// entry's read error.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick %d (post-cap): expected no error once the entry is quarantined, got %v (611.22.46)", curFailureCap+1, err)
	}
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

// TestB46SourceReadFailsThenRouteRecoversDeliversReply is the regression codex
// required: a cur entry whose source read fails identically curFailureCap
// times (EACCES), then the file is repaired (chmod restored), must deliver
// its pending reply on the next tick. The cap quarantined the source-read
// failure; once the file is readable, the entry is no longer failing, so the
// cap must not prevent the now-succeeding recovery from running.
//
// Codex P2 spec: leave the source unreadable through tick 4 (the capped
// branch skips it and returns nil, so ImportOnce sets curRecovered=true);
// repair afterward; use a separately prepared t.TempDir caller mailbox;
// check ImportOnce errors; assert the actual reply arrives.
//
// Mutation RED: without the recoverCappedCur path (the 611.22.46 NO-GO
// re-fix), once curRecovered=true the capped entry is never re-probed and the
// reply never arrives — the assertion on the reply file fails.
func TestB46SourceReadFailsThenRouteRecoversDeliversReply(t *testing.T) {
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

	// A separately prepared caller mailbox: the router resolves the cross-
	// session reply to THIS root + handle, so the reply is actually written
	// (not filepath.Dir(root), which is not a prepared AMQ root).
	callerRoot := t.TempDir()
	if err := fsq.EnsureRootDirs(callerRoot); err != nil {
		t.Fatalf("EnsureRootDirs caller: %v", err)
	}
	if err := fsq.EnsureAgentDirs(callerRoot, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs caller: %v", err)
	}
	callerNewDir := filepath.Join(callerRoot, "agents", "codex", "inbox", "new")

	carrier.SetReplyRouter(func(project, replyTo string) (string, string, error) {
		return callerRoot, "codex", nil
	})

	// Ticks 1..curFailureCap (3): source read fails (EACCES). recoverCur runs
	// (firstSweep) and reports the error; curRecovered stays false.
	for i := 1; i <= curFailureCap; i++ {
		if _, err := carrier.ImportOnce(); err == nil {
			t.Fatalf("tick %d: expected source-read error, got nil (611.22.46)", i)
		}
	}
	// Tick 4: the entry is now capped (count >= curFailureCap). The capped
	// branch skips it and returns nil, so ImportOnce sets curRecovered=true.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick 4: expected capped entry to be skipped (nil error), got: %v (611.22.46)", err)
	}
	if !carrier.curRecovered {
		t.Fatal("tick 4: curRecovered should be true after the capped pass")
	}

	// Repair the source file AFTER the silent capped pass.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod restore: %v", err)
	}

	// Tick 5: curRecovered=true, so recoverCur no longer runs. recoverCappedCur
	// (the 611.22.46 NO-GO re-fix) re-probes the capped entry, finds it
	// readable, clears the cap, and runs the full recoverOne — delivering the
	// pending reply. Check the error and assert the reply actually arrives.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick 5: expected recovery to succeed after repair, got: %v (611.22.46: capped entry must be retried once the source read succeeds)", err)
	}

	// Assert the actual reply landed in the prepared caller mailbox.
	replies, err := os.ReadDir(callerNewDir)
	if err != nil {
		t.Fatalf("read caller inbox/new: %v", err)
	}
	if len(replies) == 0 {
		t.Fatal("no reply delivered to the caller mailbox after recovery (611.22.46: a capped entry whose source is repaired must deliver its pending reply)")
	}
}

var errNoRouteForTest = errRoute("no route (test)")

type errRoute string

func (e errRoute) Error() string { return string(e) }
