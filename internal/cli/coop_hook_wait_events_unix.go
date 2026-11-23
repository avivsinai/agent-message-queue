//go:build darwin || linux

package cli

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// coopWaitEvents wakes the co-op waiter when a file it reads changes: any
// entry of the agent's inbox/new, and in the agent directory only the files
// whose change can turn a silent waiter into a ring or an exit. Other files
// of the agent directory change often under a running wake and never wake
// the waiter. With no watcher it is a plain timer, so the waiter keeps its
// poll.
type coopWaitEvents struct {
	watcher *fsnotify.Watcher
	// names are the full paths outside inbox/new whose events count.
	names map[string]bool
	inbox string
}

// newCoopWaitEvents watches <agentDir>/inbox/new and agentDir. Each of extra
// is one more file whose directory is watched, filtered to that file, when
// the directory can be watched. It never fails: a watcher that cannot be set
// up is no watcher.
func newCoopWaitEvents(agentDir string, extra ...string) *coopWaitEvents {
	e := &coopWaitEvents{
		inbox: filepath.Join(agentDir, "inbox", "new"),
		names: map[string]bool{},
	}
	// The wake's applied record names the channel it published for its
	// generation, so its change can turn a silent waiter into a ring.
	for _, name := range []string{coopWaitOwnerFile, wakeSettingsFileName, wakeSettingsAppliedFileName, wakeLockFileName, coopStopStateFile} {
		e.names[filepath.Join(agentDir, name)] = true
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return e
	}
	if watcher.Add(e.inbox) != nil || watcher.Add(agentDir) != nil {
		_ = watcher.Close()
		return e
	}
	for _, path := range extra {
		dir := filepath.Dir(path)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || watcher.Add(dir) != nil {
			continue
		}
		e.names[filepath.Join(dir, filepath.Base(path))] = true
	}
	e.watcher = watcher
	return e
}

func (e *coopWaitEvents) close() {
	if e.watcher != nil {
		_ = e.watcher.Close()
		e.watcher = nil
	}
}

// live reports whether a watcher reports file events.
func (e *coopWaitEvents) live() bool { return e.watcher != nil }

// wait returns true when d passed or a relevant event arrived, and false when
// ctx ended. It takes at most one relevant event, so an event that arrived
// while the caller worked ends the next wait at once.
func (e *coopWaitEvents) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		var (
			events <-chan fsnotify.Event
			errs   <-chan error
		)
		if e.watcher != nil {
			events, errs = e.watcher.Events, e.watcher.Errors
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case event, ok := <-events:
			if !ok {
				e.close()
				return true
			}
			if event.Name == e.inbox || filepath.Dir(event.Name) == e.inbox || e.names[event.Name] {
				return true
			}
		case _, ok := <-errs:
			if !ok {
				e.close()
			}
			return true
		}
	}
}
