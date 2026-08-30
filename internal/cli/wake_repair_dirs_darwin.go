//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

// waitRetainedWakeInboxEvent is a seam so tests can inject the interrupts the
// Go runtime delivers in production.
var waitRetainedWakeInboxEvent = func(kqueueFD int, events []unix.Kevent_t) (int, error) {
	return unix.Kevent(kqueueFD, nil, events, nil)
}

type retainedWakeInboxKqueueWatcher struct {
	kqueueFD      int
	agentFD       int
	inboxParentFD int
	inboxFD       int
	authority     retainedWakeDirectoryAuthority
	events        chan fsnotify.Event
	errors        chan error
	done          chan struct{}
	closing       chan struct{}
	close         sync.Once
	closeErr      error
}

func newRetainedWakeInboxWatcher(
	agentFD, inboxFD int,
	agentLabel, inboxLabel string,
	inboxParentIdentity wakeRepairDirectoryIdentity,
) (wakeEventWatcher, error) {
	authority, err := newRetainedWakeDirectoryAuthority(
		agentFD,
		inboxFD,
		agentLabel,
		inboxLabel,
		inboxParentIdentity,
	)
	if err != nil {
		return nil, err
	}
	inboxParentFD, err := unix.Openat(
		inboxFD,
		"..",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open retained wake inbox parent directory: %w", err)
	}
	openedParentIdentity, err := wakeRepairDirectoryIdentityForFD(
		inboxParentFD,
		"retained wake inbox parent directory",
	)
	if err != nil {
		_ = unix.Close(inboxParentFD)
		return nil, err
	}
	if openedParentIdentity != inboxParentIdentity {
		_ = unix.Close(inboxParentFD)
		return nil, fmt.Errorf(
			"retained wake inbox parent directory no longer matches original authority",
		)
	}
	kqueueFD, err := unix.Kqueue()
	if err != nil {
		_ = unix.Close(inboxParentFD)
		return nil, fmt.Errorf("create retained wake directory kqueue: %w", err)
	}
	if err := setDarwinWakeOwnerObservationCloseOnExec(
		kqueueFD,
		"retained wake directory kqueue",
	); err != nil {
		closeErr := errors.Join(unix.Close(kqueueFD), unix.Close(inboxParentFD))
		if closeErr != nil {
			return nil, fmt.Errorf("%w (close kqueue: %v)", err, closeErr)
		}
		return nil, err
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
			Ident:  uint64(inboxParentFD),
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
		_ = errors.Join(unix.Close(kqueueFD), unix.Close(inboxParentFD))
		return nil, fmt.Errorf("register retained wake directory kqueue: %w", err)
	}
	if err := authority.validateCanonical(); err != nil {
		closeErr := errors.Join(unix.Close(kqueueFD), unix.Close(inboxParentFD))
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
		kqueueFD:      kqueueFD,
		agentFD:       agentFD,
		inboxParentFD: inboxParentFD,
		inboxFD:       inboxFD,
		authority:     authority,
		events:        make(chan fsnotify.Event, 1),
		errors:        make(chan error, 1),
		done:          make(chan struct{}),
		closing:       make(chan struct{}),
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
		count, err := waitRetainedWakeInboxEvent(w.kqueueFD, events)
		if err != nil {
			select {
			case <-w.closing:
				return
			default:
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			w.fail(fmt.Errorf("wait for retained wake directory event: %w", err))
			return
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
	case w.inboxParentFD:
		return "inbox parent", nil
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
		w.closeErr = errors.Join(
			unix.Close(w.kqueueFD),
			unix.Close(w.inboxParentFD),
		)
		<-w.done
	})
	return w.closeErr
}
