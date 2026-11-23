//go:build darwin || linux

package cli

// Purpose: the co-op waiter blocks on file events and one slow tick instead
// of a one-second poll. The events test breaks if a new inbox file does not
// end a wait, or if a file of the agent directory the waiter does not read
// does. The loop test breaks if an idle waiter rings only at its slow tick.
// The hold test breaks if a wait ignores the hold deadline, which no file
// event marks; the debounce test breaks if an event cuts the debounce short.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestCoopWaitEventsWakeOnlyForFilesTheWaiterReads(t *testing.T) {
	agent := t.TempDir()
	inbox := filepath.Join(agent, "inbox", "new")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	events := newCoopWaitEvents(agent)
	defer events.close()
	if !events.live() {
		t.Fatal("no watcher on a plain agent directory")
	}
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(agent, coopWaitStateFile), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if !events.wait(ctx, 300*time.Millisecond) {
		t.Fatal("wait = false, want the timeout")
	}
	if took := time.Since(start); took < 250*time.Millisecond {
		t.Fatalf("a file the waiter does not read ended the wait after %s", took)
	}

	if err := os.WriteFile(filepath.Join(inbox, "m.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if !events.wait(ctx, 30*time.Second) {
		t.Fatal("wait = false, want an inbox event")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("a new inbox file ended the wait after %s", took)
	}
}

// coopWaitLoopForTest runs a waiter of claude in root and returns its result.
func coopWaitLoopForTest(ctx context.Context, root string) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- waitCoopMail(ctx, root, "claude", claudeWaiter("7c29e148-77eb-47c0-8984-10c13b4700b1", os.Getppid(), launch.HolderIdentity{}))
	}()
	return done
}

func coopWaitRootForTest(t *testing.T, settings string) string {
	t.Helper()
	clearSendMailboxTestEnv(t)
	// The user's machine wake settings must not set a hold or a debounce here.
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	root = canonicalWakeRoot(root)
	if settings != "" {
		if err := os.WriteFile(filepath.Join(fsq.AgentBase(root, "claude"), wakeSettingsFileName), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func sendToClaudeForTest(t *testing.T, root, body string) {
	t.Helper()
	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "codex", "--to", "claude", "--body", body})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCoopWaitIdleWaiterRingsOnArrival(t *testing.T) {
	root := coopWaitRootForTest(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := coopWaitLoopForTest(ctx, root)
	time.Sleep(400 * time.Millisecond)
	sendToClaudeForTest(t, root, "hi")
	sent := time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, errCoopWaitRing) {
			t.Fatalf("waiter = %v, want a ring", err)
		}
		if took := time.Since(sent); took > defaultWakeDebounce+2*time.Second {
			t.Fatalf("rang %s after the mail arrived", took)
		}
	case <-time.After(defaultWakeDebounce + 2*time.Second):
		t.Fatal("an idle waiter did not ring for new mail before its slow tick")
	}
}

func TestCoopWaitRingsWhenAHoldEnds(t *testing.T) {
	root := coopWaitRootForTest(t, `{"schema":1,"settings":{"hold_normal":"1500ms"}}`)
	start := time.Now()
	sendToClaudeForTest(t, root, "held")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := <-coopWaitLoopForTest(ctx, root)
	if !errors.Is(err, errCoopWaitRing) {
		t.Fatalf("waiter = %v, want a ring when the hold ends", err)
	}
	if took := time.Since(start); took < 1500*time.Millisecond {
		t.Fatalf("rang %s after the send, before the hold ended", took)
	}
}

func TestCoopWaitDebounceBatchesABurst(t *testing.T) {
	root := coopWaitRootForTest(t, `{"schema":1,"settings":{"debounce":"1s"}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := coopWaitLoopForTest(ctx, root)
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	sendToClaudeForTest(t, root, "one")
	time.Sleep(300 * time.Millisecond)
	sendToClaudeForTest(t, root, "two")
	err := <-done
	if !errors.Is(err, errCoopWaitRing) {
		t.Fatalf("waiter = %v, want a ring", err)
	}
	if took := time.Since(start); took < time.Second {
		t.Fatalf("rang %s after the first message, before the debounce passed", took)
	}
	rang := coopAnnounced(fsq.AgentBase(root, "claude"), "7c29e148-77eb-47c0-8984-10c13b4700b1", false).rang
	if len(rang.RangIDs) != 2 {
		t.Fatalf("rang ids = %v, want both messages of the burst", rang.RangIDs)
	}
}

// The review of the wait-events change found that an event just before a
// follow-up promotion was due started a full idle wait, so the promotion
// came up to twice its interval late. The wait now ends at the promotion
// time and at the hold deadline through the same bound.
func TestBoundWaitEndsAtTheEarlierTime(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name string
		at   time.Time
		want time.Duration
	}{
		{"no deadline", time.Time{}, coopWaitIdle},
		{"deadline before the wait ends", now.Add(time.Second), time.Second},
		{"deadline after the wait ends", now.Add(time.Minute), coopWaitIdle},
		{"deadline passed", now.Add(-time.Second), 0},
	} {
		if got := boundWait(coopWaitIdle, c.at, now); got != c.want {
			t.Errorf("%s: boundWait = %s, want %s", c.name, got, c.want)
		}
	}
}
