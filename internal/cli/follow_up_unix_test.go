//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestWakeLoopRingsForDueFollowUpOnMaintenanceTick(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	start := time.Unix(1_800_000_000, 0)
	timer := armFollowUpForTest(t, root, "codex", "claude", start.Add(-time.Hour), time.Hour+time.Minute)

	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	attention := make(chan string, 4)
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:              root,
			me:                "codex",
			session:           "session1",
			retainedAgent:     retainedWakeAgentDirForTest(t, root, "codex"),
			wakeOwner:         &wakeOwner{},
			injectMode:        wakeInjectModeNone,
			controlStop:       stop,
			maintenanceTicks:  ticks,
			doorbellNow:       func() time.Time { return time.Unix(0, nowNanos.Load()) },
			preconditionCheck: func(*wakeConfig) error { return nil },
			attentionIsTTY:    func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				select {
				case attention <- string(data):
				default:
				}
				return len(data), nil
			},
		})
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	}()
	tick := func() {
		t.Helper()
		select {
		case ticks <- time.Now():
		case err := <-done:
			t.Fatalf("wake loop exited: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("wake loop did not accept a maintenance tick")
		}
	}

	// A tick before the due time leaves the timer armed.
	tick()
	tick()
	if _, err := os.Stat(filepath.Join(fsq.AgentFollowUps(root, "codex"), timer.RequestID+".json")); err != nil {
		t.Fatalf("timer before due: %v, want it armed", err)
	}

	nowNanos.Store(timer.Due.UnixNano())
	tick()
	select {
	case output := <-attention:
		if !strings.Contains(output, "AMQ [session1]") {
			t.Fatalf("attention = %q, want the AMQ doorbell", output)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before ringing: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("the wake did not ring for the due follow-up")
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "codex"), timer.NoticeID+".md")); err != nil {
		t.Fatalf("notice in inbox/new: %v", err)
	}
}

func TestWaitHookRingsForDueFollowUp(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "claude,codex"}); err != nil {
		t.Fatal(err)
	}
	root = canonicalWakeRoot(root)
	timer := armFollowUpForTest(t, root, "claude", "codex", time.Now().Add(-time.Hour), 30*time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	err := waitCoopMail(ctx, root, "claude", claudeWaiter("7c29e148-77eb-47c0-8984-10c13b4700b1", os.Getppid(), launch.HolderIdentity{}))
	if !errors.Is(err, errCoopWaitRing) {
		t.Fatalf("waiter = %v, want a ring for the due follow-up", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "claude"), timer.NoticeID+".md")); err != nil {
		t.Fatalf("notice in inbox/new: %v", err)
	}
}
