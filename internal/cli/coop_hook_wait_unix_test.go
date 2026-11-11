//go:build darwin || linux

package cli

// Purpose: guards the co-op waiter, the doorbell of a session no wake
// rings. The decision test breaks if held or already announced mail rings,
// or if due mail does not. The ring test breaks if new mail does not ring
// with the fixed doorbell and the ring code, if the ring is not recorded,
// or if a later waiter rings again for the same mail.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/harness"
)

type coopWaitFileTime struct {
	os.FileInfo
	at time.Time
}

func (f coopWaitFileTime) ModTime() time.Time { return f.at }

func TestCoopWaitRingsOnlyForDueMailNoHookAnnounced(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	arrived := coopWaitFileTime{at: now.Add(-time.Minute)}
	policy := wakeHoldPolicy{normal: 5 * time.Minute, low: 30 * time.Minute}
	pending := []coopStopMessage{
		{ID: "urgent", Priority: format.PriorityUrgent, File: arrived},
		{ID: "normal", Priority: format.PriorityNormal, File: arrived},
		{ID: "low", Priority: format.PriorityLow, File: arrived},
		{ID: "reported", Priority: format.PriorityUrgent, File: arrived},
	}

	due, next := decideCoopWait(pending, []string{"reported"}, policy, now)
	if !slices.Equal(due, []string{"urgent"}) || !next.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("due = %v next = %v, want urgent now and normal in 4m", due, next)
	}
	due, next = decideCoopWait(pending, []string{"reported"}, wakeHoldPolicy{}, now)
	if !slices.Equal(due, []string{"low", "normal", "urgent"}) || !next.IsZero() {
		t.Fatalf("without holds: due = %v next = %v, want all three and nothing held", due, next)
	}
}

func TestCoopWaitRingsOnceForNewMail(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	root = canonicalWakeRoot(root)
	const claudeSession = "7c29e148-77eb-47c0-8984-10c13b4700b1"
	// wait runs one waiter for at most limit. A waiter scans at once and
	// rings one debounce later, so a short limit shows that it does not ring.
	wait := func(limit time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		return waitCoopMail(ctx, root, "claude", claudeSession, os.Getppid())
	}
	quiet := 2 * defaultWakeDebounce
	if err := wait(quiet); err != nil {
		t.Fatalf("empty inbox: %v, want no ring", err)
	}
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "codex", "--to", "claude", "--body", "hi"})
	}); err != nil {
		t.Fatal(err)
	}

	err := wait(4 * time.Second)
	if !errors.Is(err, errCoopWaitRing) || GetExitCode(err) != harness.ClaudeRewakeRingExit || err.Error() != harness.Doorbell {
		t.Fatalf("new mail: %v (exit %d), want the doorbell with the ring code", err, GetExitCode(err))
	}
	statePath := filepath.Join(root, "agents", "claude", coopWaitStateFile)
	if rang := readCoopWaitState(statePath, claudeSession); len(rang.RangIDs) != 1 {
		t.Fatalf("rang ids = %v, want the one message", rang.RangIDs)
	}
	// A ring is what one session heard: another session that takes the
	// handle must not inherit it.
	if rang := readCoopWaitState(statePath, "0c29e148-77eb-47c0-8984-10c13b4700b2"); len(rang.RangIDs) != 0 {
		t.Fatalf("another session reads rang ids %v, want none", rang.RangIDs)
	}
	if err := wait(quiet); err != nil {
		t.Fatalf("same mail again: %v, want no second ring", err)
	}
}
