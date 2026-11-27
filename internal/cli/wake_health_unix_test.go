//go:build darwin || linux

package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

// Regression: the 2026-10-26 machine audit found live wakes whose failed
// watchers still passed wake check. Existing lock tests only proved PID identity.
func TestWakeHealthDetectsClosedWatcherAndRecoveryDeliversMail(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	var attention strings.Builder
	l := &wakeLoop{
		cfg: wakeConfig{
			root: root, me: "codex", injectMode: wakeInjectModeNone,
			settings:       wakeSettings{previewLen: 80},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) { return attention.Write(data) },
		},
		inboxNew:            fsq.AgentInboxNew(root, "codex"),
		ordinaryRecoverable: true,
		ordinaryAgentDir:    retainedWakeAgentDirForTest(t, root, "codex"),
	}
	defer l.closeWatchers()
	defer l.stopTimers()
	if err := l.openMainWatcher(); err != nil {
		t.Fatal(err)
	}
	if got := l.healthStatus(); got != wakeHealthHealthy {
		t.Fatalf("armed watcher health = %s", got)
	}
	if err := l.watcher.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.watcher.Errors():
	case <-time.After(time.Second):
		t.Fatal("watcher did not finish closing")
	}
	if got := l.healthStatus(); got != wakeHealthDegraded {
		t.Fatalf("closed watcher health = %s, want degraded", got)
	}
	if l.watcher != nil || l.inboxScanRetryC == nil {
		t.Fatal("failed watcher did not enter retry")
	}
	l.inboxScanRetryC = nil
	if err := l.onInboxScanRetry(); err != nil {
		t.Fatal(err)
	}
	if got := l.healthStatus(); got != wakeHealthHealthy {
		t.Fatalf("rearmed watcher health = %s", got)
	}
	deliverWakeWatcherMessageForTest(t, root, "codex", "after-rearm", "claude")
	select {
	case event := <-l.watcher.Events():
		l.onWatcherEvent(event)
	case <-time.After(time.Second):
		t.Fatal("rearmed watcher missed a new message")
	}
	if err := l.attemptNotification(); err != nil {
		t.Fatal(err)
	}
	if attention.Len() == 0 {
		t.Fatal("rearmed wake did not notify for new mail")
	}
}

// Issue #1660: passive filesystem arrivals must leave the debounce alone,
// while urgent mail must release a burst immediately.
func TestWakeDebounceIgnoresPassiveAndReleasesUrgent(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	l := wakeLoop{cfg: *h.cfg}
	l.cfg.settings.debounce = time.Hour
	events := 0
	l.cfg.onPendingNotify = func() { events++ }
	defer l.stopTimers()
	create := func(id, priority, notify string) {
		h.deliver(id+".md", id, priority, notify, nil)
		l.onWatcherEvent(fsnotify.Event{
			Name: filepath.Join(fsq.AgentInboxNew(h.root, "codex"), id+".md"),
			Op:   fsnotify.Create,
		})
	}
	create("work", format.PriorityNormal, "")
	timer := l.debounceTimer
	if timer == nil {
		t.Fatal("normal mail did not arm debounce")
	}
	create("fyi", format.PriorityNormal, format.NotifyPassive)
	if events != 1 || l.debounceTimer != timer {
		t.Fatal("passive event rescheduled debounce")
	}
	create("urgent", format.PriorityUrgent, "")
	select {
	case <-l.debounceTimer.C:
	case <-time.After(time.Second):
		t.Fatal("urgent event did not release debounce")
	}
}
