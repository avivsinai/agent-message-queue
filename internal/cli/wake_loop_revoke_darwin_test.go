//go:build darwin

package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

type closedWakeEventWatcher struct {
	events chan fsnotify.Event
	errors chan error
}

func (watcher closedWakeEventWatcher) Events() <-chan fsnotify.Event { return watcher.events }
func (watcher closedWakeEventWatcher) Errors() <-chan error          { return watcher.errors }
func (closedWakeEventWatcher) Close() error                          { return nil }

// #1312: a wake stayed alive and deaf after its root volume was revoked. The
// kernel sends one NOTE_REVOKE kevent; the watcher buffers its error and then
// closes both channels, so the loop may see either channel close first.
func TestWakeLoopExitsWhenRetainedDirectoryRevoked(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	var attention strings.Builder
	watcher := closedWakeEventWatcher{
		events: make(chan fsnotify.Event),
		errors: make(chan error, 1),
	}
	watcher.errors <- retainedWakeKeventError("agent", unix.NOTE_REVOKE)
	close(watcher.errors)
	close(watcher.events)
	stop := make(chan struct{})
	l := &wakeLoop{
		cfg: wakeConfig{
			root: root, me: "codex", injectMode: wakeInjectModeNone,
			settings:       wakeSettings{previewLen: 80},
			controlStop:    stop,
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) { return attention.Write(data) },
		},
		inboxNew: fsq.AgentInboxNew(root, "codex"),
		watcher:  watcher,
	}
	defer l.closeWatchers()
	defer l.stopTimers()

	result := make(chan error, 1)
	go func() { result <- l.run(nil, nil) }()
	select {
	case err := <-result:
		if !errors.Is(err, errWakeDirectoryRevoked) {
			t.Fatalf("run returned %v, want errWakeDirectoryRevoked", err)
		}
	case <-time.After(2 * time.Second):
		close(stop)
		<-result
		t.Fatal("wake loop kept running after its retained directory was revoked")
	}
}
