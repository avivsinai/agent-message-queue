//go:build darwin || linux

package cli

// Purpose: guards the co-op waiter, the doorbell of a session no wake
// rings. The decision test breaks if held or already announced mail rings,
// or if due mail does not. The ring test breaks if new mail does not ring
// with the fixed doorbell and the ring code, if the ring is not recorded,
// or if a later waiter rings again for the same mail. The presence test
// breaks if an agent whose waiter runs reads as stale to its peers, as a
// joined desktop-app session did, or if looking creates the waiter's lock;
// pre-merge review added that a lock path that is a link is not followed.
// The last two tests come from a measurement of the hooks' cost: a waiter
// read the header of every unread message again at each poll, for as long
// as the mail stayed unread, and a waiter whose Claude Code process was
// killed kept running, because the shell of its hook survives that process.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
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
		return waitCoopMail(ctx, root, "claude", claudeSession, os.Getppid(), launch.HolderIdentity{})
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

func TestAnAgentWhoseWaiterRunsIsActive(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "claude"), coopWaitLockFile)
	if got := resolvePresenceSource(root, "claude", false); got != "" {
		t.Fatalf("an agent no waiter ran for has presence source %q, want none", got)
	}
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("looking at presence created the waiter lock: %v", err)
	}
	release, ok, err := lock.TryExclusive(lockPath)
	if err != nil || !ok {
		t.Fatalf("take the waiter lock: ok=%v err=%v", ok, err)
	}
	if got := resolvePresenceSource(root, "claude", false); got != presenceSourceHookLive {
		t.Fatalf("an agent whose waiter runs has presence source %q, want %q", got, presenceSourceHookLive)
	}
	release()
	if got := resolvePresenceSource(root, "claude", false); got != "" {
		t.Fatalf("an agent whose waiter ended has presence source %q, want none", got)
	}

	// A lock path that is a link names a file outside the root. Looking
	// must not create or lock it.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, lockPath); err != nil {
		t.Fatal(err)
	}
	if got := resolvePresenceSource(root, "claude", false); got != "" {
		t.Fatalf("a lock path that is a link gives presence source %q, want none", got)
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("looking at presence created the link's target: %v", err)
	}
}

func TestCoopInboxReadsEachUnreadMessageOnce(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "codex", "--to", "claude", "--body", "hi"})
	}); err != nil {
		t.Fatal(err)
	}
	var inbox coopInbox
	first, err := inbox.pending(root, "claude")
	if err != nil || len(first) != 1 {
		t.Fatalf("pending = %v %v, want the one message", first, err)
	}
	files, err := filepath.Glob(filepath.Join(fsq.AgentInboxNew(root, "claude"), "*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("inbox files = %v %v", files, err)
	}
	// A message file never changes, so a second look that read it again
	// would see this as a corrupt file and drop the message.
	if err := os.WriteFile(files[0], []byte("not a message"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := inbox.pending(root, "claude"); err != nil || len(again) != 1 || again[0].ID != first[0].ID {
		t.Fatalf("second look = %v %v, want the same message with no second read", again, err)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if gone, err := inbox.pending(root, "claude"); err != nil || len(gone) != 0 {
		t.Fatalf("after the message left = %v %v, want none", gone, err)
	}
}

func TestCoopWaitEndsWhenItsClaudeProcessIsGone(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	root = canonicalWakeRoot(root)
	// A process stands for the session's Claude Code. The waiter is not
	// its child here, as the hook's shell is not after that process dies.
	claude := exec.Command("sleep", "60")
	if err := claude.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := launch.HolderOf(claude.Process.Pid)
	if err != nil {
		_ = claude.Process.Kill()
		t.Fatal(err)
	}
	_ = claude.Process.Kill()
	_ = claude.Wait()

	const limit = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()
	if err := waitCoopMail(ctx, root, "claude", "7c29e148-77eb-47c0-8984-10c13b4700b1", os.Getppid(), identity); err != nil {
		t.Fatalf("waiter of a dead Claude process: %v, want a quiet end", err)
	}
	if waited := time.Since(start); waited > limit/2 {
		t.Fatalf("the waiter ran %s after its Claude process was gone, want it to end at once", waited)
	}
}
