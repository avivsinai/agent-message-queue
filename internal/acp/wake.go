package acp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// The machine agent's workers pass when something they read changes, not
// on a fixed tick. Each worker watches the directories and files its pass
// reads; a change wakes it, but never sooner than its base interval after
// its last pass began, so a burst of changes makes one pass and a worker
// never passes more often than a fixed tick would. A pass that leaves work
// in flight (an open dialog, a queued edit, a channel being set up, a
// backoff) asks for the base interval. A settled pass waits for a change,
// for the deadline it names, or for its idle poll, which also catches what no
// watch sees: a session that ended, a dropped event, a path that could not
// be watched. Without a watcher, or while a path cannot be watched, the
// worker passes every base interval, as with a tick.

// The longest a settled worker waits for a change. The machine worker's
// is shorter: it sees a session whose process exited, and its archive, at
// this pace. It is at most lateReplyInterval and channelStepGap, so the
// machine worker's periodic steps stay on time.
var (
	idlePoll        = 30 * time.Second
	machineIdlePoll = 10 * time.Second
)

// newWatcher makes a worker's file watcher; a variable so tests can fail
// it.
var newWatcher = fsnotify.NewWatcher

// pace is what a pass asks of its worker's wait.
type pace struct {
	// next is when the next pass is due at the latest; zero waits for a
	// change or the idle poll.
	next time.Time
	// watch is every path whose change may need a pass.
	watch []string
}

// due keeps the earlier of p's deadline and at.
func (p *pace) due(at time.Time) {
	if p.next.IsZero() || at.Before(p.next) {
		p.next = at
	}
}

// wakeup paces one worker.
type wakeup struct {
	name  string
	every time.Duration
	// idle is the longest a settled worker waits for a change.
	idle time.Duration
	w    *fsnotify.Watcher
	kick chan struct{}
	// lost closes when the watcher stops delivering events.
	lost chan struct{}
	// partial is set while a wanted path exists but could not be watched;
	// warned is set once that was said.
	partial, warned bool
	closing         atomic.Bool
}

// polling paces a worker that passes every base interval.
func polling(name string, every time.Duration) *wakeup {
	u := &wakeup{name: name, every: every, kick: make(chan struct{}, 1), lost: make(chan struct{})}
	close(u.lost)
	return u
}

// newWakeup starts the watcher of the worker name, whose base interval is
// every and whose idle poll is idle. A watcher that cannot start leaves the worker polling every base
// interval, and says why once.
func newWakeup(name string, every, idle time.Duration) *wakeup {
	w, err := newWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "amq-acp: the %s worker cannot watch files (%v); it polls every %s\n", name, err, every)
		return polling(name, every)
	}
	u := &wakeup{name: name, every: every, idle: idle, w: w, kick: make(chan struct{}, 1), lost: make(chan struct{})}
	go u.forward()
	return u
}

// forward turns every event and every watcher error (an overflow drops
// events) into one pending wake. A watcher that stops on its own leaves
// the worker polling every base interval, and says so.
func (u *wakeup) forward() {
	defer close(u.lost)
	defer func() {
		if !u.closing.Load() {
			fmt.Fprintf(os.Stderr, "amq-acp: the %s worker's file watch stopped; it polls every %s\n", u.name, u.every)
		}
	}()
	for {
		select {
		case _, ok := <-u.w.Events:
			if !ok {
				return
			}
		case _, ok := <-u.w.Errors:
			if !ok {
				return
			}
		}
		select {
		case u.kick <- struct{}{}:
		default:
		}
	}
}

func (u *wakeup) close() {
	if u.w != nil {
		u.closing.Store(true)
		_ = u.w.Close()
		<-u.lost
	}
}

// watching reports whether changes wake the worker.
func (u *wakeup) watching() bool {
	select {
	case <-u.lost:
		return false
	default:
		return u.w != nil && !u.partial
	}
}

// watch makes the watched set paths: it adds the ones missing and removes
// the rest. A path that does not exist yet is tried again on the next
// pass; its parent's watch sees it appear. grew reports a path watched now
// that was not before: what changed in it after the pass read it and
// before the watch began is unseen, so the worker passes again.
func (u *wakeup) watch(paths []string) (grew bool) {
	if u.w == nil {
		return false
	}
	want := map[string]bool{}
	for _, p := range paths {
		if p != "" {
			want[filepath.Clean(p)] = true
		}
	}
	held := map[string]bool{}
	for _, p := range u.w.WatchList() {
		if want[p] {
			held[p] = true
		} else {
			_ = u.w.Remove(p)
		}
	}
	u.partial = false
	for p := range want {
		if held[p] {
			continue
		}
		err := u.w.Add(p)
		switch {
		case err == nil:
			grew = true
		case errors.Is(err, fs.ErrNotExist):
		default:
			u.partial = true
			if !u.warned {
				u.warned = true
				fmt.Fprintf(os.Stderr, "amq-acp: the %s worker cannot watch %s (%v); it polls every %s while a path cannot be watched\n", u.name, p, err, u.every)
			}
		}
	}
	return grew
}

// wait watches p's paths and returns once the next pass may run, false
// when quit closed. A pass began at started.
func (u *wakeup) wait(quit <-chan struct{}, started time.Time, p pace) bool {
	grew := u.watch(p.watch)
	earliest := started.Add(u.every)
	at := earliest
	if u.watching() && !grew {
		at = started.Add(u.idle)
		if !p.next.IsZero() && p.next.Before(at) {
			at = p.next
		}
		if at.Before(earliest) {
			at = earliest
		}
	}
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-quit:
		return false
	case <-timer.C:
		return true
	case <-u.kick:
	case <-u.lost:
	}
	if !timer.Stop() {
		return true
	}
	timer.Reset(time.Until(earliest))
	select {
	case <-quit:
		return false
	case <-timer.C:
		return true
	}
}

// sessionWatch is what every worker's pass reads to find the sessions it
// serves: the binding file's directory, the bindings directory and the
// channel records.
func (s *Server) sessionWatch() []string {
	file, dir, err := binding.WatchPaths()
	if err != nil {
		return []string{s.channelDir()}
	}
	return []string{filepath.Dir(file), dir, s.channelDir()}
}
