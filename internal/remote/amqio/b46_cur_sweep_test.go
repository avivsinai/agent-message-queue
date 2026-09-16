package amqio

import (
	"fmt"
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

// TestB46PersistentlyFailingCurEntryStopsResweeping reproduces
// agent-message-queue-611.22.46: one cur entry whose read always fails kept
// curRecovered false forever, so recoverCur ran a full O(cur) sweep every
// tick and re-processed the same failing entry each time. The fix tracks
// consecutive identical read failures per entry; once an entry fails
// identically curFailureCap times, it is skipped on subsequent sweeps
// (quarantined from the sweep, not from correctness — amq tooling owns DLQ).
// A DIFFERENT error resets the count so a transient EAGAIN that later changes
// still retries.
//
// Mutation RED: remove the curFailureCapped skip in recoverCur -> the entry
// is re-processed every tick and ImportOnce keeps returning its error past
// the cap, so the "no error after cap" assertion fails.
func TestB46PersistentlyFailingCurEntryStopsResweeping(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, DefaultHandle); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// A cur entry whose reply route always fails — recoverOne routes the
	// recovery reply and fails, the same way TestB8CurRecoveredNotSetOnFailure
	// drives a per-entry failure.
	curDir := filepath.Join(root, "agents", DefaultHandle, "inbox", "cur")
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		t.Fatalf("mkdir cur: %v", err)
	}
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:       format.CurrentSchema,
			ID:           id,
			From:         "codex",
			To:           []string{DefaultHandle},
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
	if err := os.WriteFile(filepath.Join(curDir, id+".md"), data, 0o600); err != nil {
		t.Fatalf("write cur: %v", err)
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
	carrier.SetReplyRouter(func(project, replyTo string) (string, string, error) {
		return "", "", fmt.Errorf("no route")
	})

	// Ticks 1..curFailureCap: the entry is processed and fails each time.
	// ImportOnce returns an error (the entry's read/route failure).
	for i := 1; i <= curFailureCap; i++ {
		if _, err := carrier.ImportOnce(); err == nil {
			t.Fatalf("tick %d: expected an error from the failing cur entry, got nil", i)
		}
	}

	// Tick curFailureCap+1: the entry is now quarantined from the sweep
	// (identical failure count >= curFailureCap). recoverOne is NOT called, so
	// ImportOnce no longer reports the entry's error.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick %d (post-cap): expected no error once the entry is quarantined, got %v (611.22.46)", curFailureCap+1, err)
	}

	// The sweep still ran (curSweepCount increments each tick — the entry is
	// skipped, not the sweep), but the capped entry contributed no error.
	carrier.mu.Lock()
	sweeps := carrier.curSweepCount
	capped := carrier.curFailures[id+".md"]
	carrier.mu.Unlock()
	if sweeps < curFailureCap+1 {
		t.Fatalf("expected at least %d sweeps, got %d", curFailureCap+1, sweeps)
	}
	if capped.count < curFailureCap {
		t.Fatalf("failure count = %d, want >= %d (611.22.46)", capped.count, curFailureCap)
	}
}

// TestB46TransientChangedErrorRetries proves the bead's transient invariant:
// a DIFFERENT error on a subsequent sweep resets the consecutive-identical
// failure count, so a transient fault that changes character still retries
// instead of being permanently quarantined.
//
// Mutation RED: make recordCurFailure increment regardless of whether the
// error changed (drop the lastErr comparison) -> the entry is quarantined
// after curFailureCap total failures even though each was a different error,
// and the test fails (the entry stops being retried before it should).
func TestB46TransientChangedErrorRetries(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, DefaultHandle); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	curDir := filepath.Join(root, "agents", DefaultHandle, "inbox", "cur")
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		t.Fatalf("mkdir cur: %v", err)
	}
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:       format.CurrentSchema,
			ID:           id,
			From:         "codex",
			To:           []string{DefaultHandle},
			Thread:       "p2p/codex__remote",
			Subject:      "submit",
			Created:      now.UTC().Format(time.RFC3339Nano),
			Kind:         "todo",
			ReplyProject: "nonexistent-project",
			ReplyTo:      "codex@nonexistent-project",
		},
		Body: `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111462","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"work"}}`,
	}
	data, _ := msg.Marshal()
	if err := os.WriteFile(filepath.Join(curDir, id+".md"), data, 0o600); err != nil {
		t.Fatalf("write cur: %v", err)
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

	// Rotate the router error so each sweep fails with a DIFFERENT message.
	errs := []string{"no route A", "no route B", "no route C", "no route D", "no route E", "no route F"}
	i := 0
	carrier.SetReplyRouter(func(project, replyTo string) (string, string, error) {
		e := errs[i%len(errs)]
		i++
		return "", "", fmt.Errorf("%s", e)
	})

	// Run more than curFailureCap ticks with a different error each time. The
	// entry must NEVER be quarantined, because no two consecutive failures are
	// identical (the count resets to 1 each time the error changes).
	for tick := 1; tick <= curFailureCap+2; tick++ {
		_, _ = carrier.ImportOnce()
		carrier.mu.Lock()
		f := carrier.curFailures[id+".md"]
		carrier.mu.Unlock()
		if f.count >= curFailureCap {
			t.Fatalf("tick %d: entry quarantined with count %d, but errors changed each tick (611.22.46 transient invariant)", tick, f.count)
		}
	}
}
