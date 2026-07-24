//go:build linux

package cli

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type retainedWakeInboxFSNotifyWatcher struct {
	watcher  *fsnotify.Watcher
	events   chan fsnotify.Event
	errors   chan error
	done     chan struct{}
	close    sync.Once
	closeErr error
}

func newRetainedWakeInboxWatcher(dirfd int, label string) (wakeEventWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create retained wake inbox watcher: %w", err)
	}
	descriptorPath := fmt.Sprintf("/proc/self/fd/%d", dirfd)
	if err := watcher.Add(descriptorPath); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("watch retained wake inbox descriptor: %w", err)
	}
	retained := &retainedWakeInboxFSNotifyWatcher{
		watcher: watcher,
		events:  make(chan fsnotify.Event, 1),
		errors:  make(chan error, 1),
		done:    make(chan struct{}),
	}
	go retained.run(label)
	return retained, nil
}

func (w *retainedWakeInboxFSNotifyWatcher) run(label string) {
	defer close(w.done)
	defer close(w.events)
	defer close(w.errors)
	eventName := filepath.Join(label, "retained-inbox-event.md")
	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
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
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			select {
			case w.errors <- fmt.Errorf("watch retained wake inbox: %w", err):
			default:
			}
			return
		}
	}
}

func (w *retainedWakeInboxFSNotifyWatcher) Events() <-chan fsnotify.Event {
	return w.events
}

func (w *retainedWakeInboxFSNotifyWatcher) Errors() <-chan error {
	return w.errors
}

func (w *retainedWakeInboxFSNotifyWatcher) Close() error {
	w.close.Do(func() {
		w.closeErr = w.watcher.Close()
		<-w.done
	})
	return w.closeErr
}
