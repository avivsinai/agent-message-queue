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
	kqueueFD  int
	agentFD   int
	inboxFD   int
	authority retainedWakeDirectoryAuthority
	events    chan fsnotify.Event
	errors    chan error
	done      chan struct{}
	closing   chan struct{}
	close     sync.Once
	closeErr  error
}

func newRetainedWakeInboxWatcher(
	agentFD, inboxFD int,
	agentLabel, inboxLabel string,
) (wakeEventWatcher, error) {
	authority, err := newRetainedWakeDirectoryAuthority(
		agentFD,
		inboxFD,
		agentLabel,
		inboxLabel,
	)
	if err != nil {
		return nil, err
	}
	kqueueFD, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("create retained wake directory kqueue: %w", err)
	}
	flags := uint32(
		unix.NOTE_WRITE |
			unix.NOTE_EXTEND |
			unix.NOTE_ATTRIB |
			unix.NOTE_LINK |
			unix.NOTE_RENAME |
			unix.NOTE_DELETE |
			unix.NOTE_REVOKE,
	)
	changes := []unix.Kevent_t{
		{
			Ident:  uint64(agentFD),
			Filter: unix.EVFILT_VNODE,
			Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR,
			Fflags: flags,
		},
		{
			Ident:  uint64(inboxFD),
			Filter: unix.EVFILT_VNODE,
			Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR,
			Fflags: flags,
		},
	}
	if _, err := unix.Kevent(kqueueFD, changes, nil, nil); err != nil {
		_ = unix.Close(kqueueFD)
		return nil, fmt.Errorf("register retained wake directory kqueue: %w", err)
	}
	if err := authority.validateCanonical(); err != nil {
		closeErr := unix.Close(kqueueFD)
		if closeErr != nil {
			return nil, fmt.Errorf(
				"validate retained wake directories after watch registration: %w (close kqueue: %v)",
				err,
				closeErr,
			)
		}
		return nil, fmt.Errorf(
			"validate retained wake directories after watch registration: %w",
			err,
		)
	}
	watcher := &retainedWakeInboxKqueueWatcher{
		kqueueFD:  kqueueFD,
		agentFD:   agentFD,
		inboxFD:   inboxFD,
		authority: authority,
		events:    make(chan fsnotify.Event, 1),
		errors:    make(chan error, 1),
		done:      make(chan struct{}),
		closing:   make(chan struct{}),
	}
	go watcher.run()
	return watcher, nil
}

func (w *retainedWakeInboxKqueueWatcher) run() {
	defer close(w.done)
	defer close(w.events)
	defer close(w.errors)
	eventName := filepath.Join(w.authority.inboxPath, "retained-inbox-event.md")
	for {
		events := make([]unix.Kevent_t, 2)
		count, err := unix.Kevent(w.kqueueFD, nil, events, nil)
		if err != nil {
			select {
			case <-w.closing:
				return
			default:
				w.fail(fmt.Errorf("wait for retained wake directory event: %w", err))
				return
			}
		}
		if count == 0 {
			continue
		}

		inboxEvent := false
		for _, event := range events[:count] {
			source, sourceErr := w.eventSource(event.Ident)
			if sourceErr != nil {
				w.fail(sourceErr)
				return
			}
			if event.Fflags&(unix.NOTE_RENAME|unix.NOTE_DELETE|unix.NOTE_REVOKE) != 0 {
				w.fail(fmt.Errorf("retained wake %s directory was renamed or deleted", source))
				return
			}
			if source == "inbox" {
				inboxEvent = true
			}
		}
		if err := w.authority.validateCanonical(); err != nil {
			w.fail(fmt.Errorf("retained wake directory namespace validation failed: %w", err))
			return
		}
		if inboxEvent {
			select {
			case w.events <- fsnotify.Event{Name: eventName, Op: fsnotify.Write}:
			default:
			}
		}
	}
}

func (w *retainedWakeInboxKqueueWatcher) eventSource(ident uint64) (string, error) {
	switch int(ident) {
	case w.agentFD:
		return "agent", nil
	case w.inboxFD:
		return "inbox", nil
	default:
		return "", fmt.Errorf("retained wake kqueue returned unknown directory descriptor %d", ident)
	}
}

func (w *retainedWakeInboxKqueueWatcher) fail(err error) {
	select {
	case w.errors <- err:
	default:
	}
}

func (w *retainedWakeInboxKqueueWatcher) Events() <-chan fsnotify.Event {
	return w.events
}

func (w *retainedWakeInboxKqueueWatcher) Errors() <-chan error {
	return w.errors
}

func (w *retainedWakeInboxKqueueWatcher) Close() error {
	w.close.Do(func() {
		close(w.closing)
		w.closeErr = unix.Close(w.kqueueFD)
		<-w.done
	})
	return w.closeErr
}
