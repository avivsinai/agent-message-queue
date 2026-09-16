package amqio

import (
	"os"
	"path/filepath"
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
// Mutation RED: if the cap permanently skipped the entry even after the file
// is repaired (no way to observe recovery), the pending reply would never be
// delivered and the assertion fails.
func TestB46SourceReadFailsThenRouteRecoversDeliversReply(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions; EACCES cannot be simulated")
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

	// Track whether the reply route is reached (recovery proceeded past the
	// source read) and succeeds.
	routeReached := false
	carrier.SetReplyRouter(func(project, replyTo string) (string, string, error) {
		routeReached = true
		return filepath.Dir(root), "codex", nil
	})

	// Ticks 1..curFailureCap: source read fails (EACCES). No reply yet.
	for i := 1; i <= curFailureCap; i++ {
		_, _ = carrier.ImportOnce()
		if routeReached {
			t.Fatalf("tick %d: route reached before file repaired", i)
		}
	}

	// Repair the source file: the entry is now readable.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod restore: %v", err)
	}

	// The entry was capped on the previous tick. The cap must not permanently
	// suppress a now-readable entry: clearCurFailure fires on a successful
	// read, so the recovery must proceed and deliver the pending reply.
	//
	// NOTE: the cap skips the entry via curFailureCapped BEFORE recoverOne. To
	// observe recovery, the cap must be cleared when the underlying condition
	// changes. clearCurFailure is called on success, but a capped entry is
	// skipped before recoverOne runs — so the cap must be reset to let the
	// repaired entry be re-examined. The repair itself (chmod) is an external
	// state change the sweep cannot detect without re-reading. We re-run a few
	// ticks; the implementation must clear the cap so the entry is retried.
	for i := 1; i <= 2; i++ {
		_, _ = carrier.ImportOnce()
		if routeReached {
			break
		}
	}
	if !routeReached {
		t.Fatal("pending reply was never delivered after the source file was repaired (611.22.46: a capped entry must be retried once the source read succeeds, not permanently suppressed)")
	}
}

var errNoRouteForTest = errRoute("no route (test)")

type errRoute string

func (e errRoute) Error() string { return string(e) }
