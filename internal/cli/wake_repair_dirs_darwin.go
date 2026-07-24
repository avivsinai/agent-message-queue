//go:build darwin

package cli

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

type retainedWakeInboxKqueueWatcher struct {
	kqueueFD int
	events   chan fsnotify.Event
	errors   chan error
	done     chan struct{}
	close    sync.Once
}

func newRetainedWakeInboxWatcher(dirfd int, label string) (wakeEventWatcher, error) {
	kqueueFD, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("create retained wake inbox kqueue: %w", err)
	}
	change := unix.Kevent_t{
		Ident:  uint64(dirfd),
		Filter: unix.EVFILT_VNODE,
		Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR,
		Fflags: unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_ATTRIB | unix.NOTE_LINK | unix.NOTE_RENAME | unix.NOTE_DELETE,
	}
	if _, err := unix.Kevent(kqueueFD, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(kqueueFD)
		return nil, fmt.Errorf("register retained wake inbox kqueue: %w", err)
	}
	watcher := &retainedWakeInboxKqueueWatcher{
		kqueueFD: kqueueFD,
		events:   make(chan fsnotify.Event, 1),
		errors:   make(chan error, 1),
		done:     make(chan struct{}),
	}
	go watcher.run(label)
	return watcher, nil
}

func (w *retainedWakeInboxKqueueWatcher) run(label string) {
	defer close(w.done)
	defer close(w.events)
	defer close(w.errors)
	eventName := filepath.Join(label, "retained-inbox-event.md")
	for {
		events := make([]unix.Kevent_t, 1)
		count, err := unix.Kevent(w.kqueueFD, nil, events, nil)
		if err != nil {
			if err != unix.EBADF && err != unix.EINVAL {
				select {
				case w.errors <- fmt.Errorf("wait for retained wake inbox event: %w", err):
				default:
				}
			}
			return
		}
		if count == 0 {
			continue
		}
		if events[0].Fflags&(unix.NOTE_RENAME|unix.NOTE_DELETE) != 0 {
			select {
			case w.errors <- fmt.Errorf("retained wake inbox directory was renamed or deleted"):
			default:
			}
			return
		}
		select {
		case w.events <- fsnotify.Event{Name: eventName, Op: fsnotify.Write}:
		default:
		}
	}
}

func (w *retainedWakeInboxKqueueWatcher) Events() <-chan fsnotify.Event {
	return w.events
}

func (w *retainedWakeInboxKqueueWatcher) Errors() <-chan error {
	return w.errors
}

func (w *retainedWakeInboxKqueueWatcher) Close() error {
	var closeErr error
	w.close.Do(func() {
		closeErr = unix.Close(w.kqueueFD)
		<-w.done
	})
	return closeErr
}
