//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

type retainedWakeInboxFSNotifyWatcher struct {
	namespaceWatcher *fsnotify.Watcher
	inboxWatcher     *fsnotify.Watcher
	inboxParent      *os.File
	authority        retainedWakeDirectoryAuthority
	events           chan fsnotify.Event
	errors           chan error
	done             chan struct{}
	close            sync.Once
	closeErr         error
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
	inboxParentFD, err := unix.Openat(
		inboxFD,
		"..",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open retained wake inbox parent directory: %w", err)
	}
	inboxParent := os.NewFile(
		uintptr(inboxParentFD),
		filepath.Dir(inboxLabel),
	)
	namespaceWatcher, err := newRetainedWakeNamespaceFSNotifyWatcher(
		agentFD,
		inboxParentFD,
	)
	if err != nil {
		_ = inboxParent.Close()
		return nil, err
	}
	inboxWatcher, err := newRetainedWakeFSNotifyWatcher(
		inboxFD,
		"inbox",
	)
	if err != nil {
		_ = namespaceWatcher.Close()
		_ = inboxParent.Close()
		return nil, err
	}
	if err := authority.validateCanonical(); err != nil {
		closeErr := errors.Join(
			namespaceWatcher.Close(),
			inboxWatcher.Close(),
			inboxParent.Close(),
		)
		return nil, errors.Join(
			fmt.Errorf("validate retained wake directories after watch registration: %w", err),
			closeErr,
		)
	}
	retained := &retainedWakeInboxFSNotifyWatcher{
		namespaceWatcher: namespaceWatcher,
		inboxWatcher:     inboxWatcher,
		inboxParent:      inboxParent,
		authority:        authority,
		events:           make(chan fsnotify.Event, 1),
		errors:           make(chan error, 1),
		done:             make(chan struct{}),
	}
	go retained.run()
	return retained, nil
}

func newRetainedWakeNamespaceFSNotifyWatcher(
	agentFD, inboxParentFD int,
) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create retained wake namespace watcher: %w", err)
	}
	if err := addRetainedWakeFSNotifyDescriptor(watcher, agentFD, "agent"); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	if err := addRetainedWakeFSNotifyDescriptor(watcher, inboxParentFD, "inbox parent"); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	return watcher, nil
}

func newRetainedWakeFSNotifyWatcher(
	fd int,
	label string,
) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create retained wake %s watcher: %w", label, err)
	}
	if err := addRetainedWakeFSNotifyDescriptor(watcher, fd, label); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	return watcher, nil
}

func addRetainedWakeFSNotifyDescriptor(
	watcher *fsnotify.Watcher,
	fd int,
	label string,
) error {
	descriptorPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	if err := watcher.Add(descriptorPath); err != nil {
		return fmt.Errorf("watch retained wake %s descriptor: %w", label, err)
	}
	return nil
}

func (w *retainedWakeInboxFSNotifyWatcher) run() {
	defer close(w.done)
	defer close(w.events)
	defer close(w.errors)
	eventName := filepath.Join(w.authority.inboxPath, "retained-inbox-event.md")
	for {
		select {
		case _, ok := <-w.namespaceWatcher.Events:
			if !ok {
				return
			}
			if err := w.authority.validateCanonical(); err != nil {
				w.fail(fmt.Errorf("retained wake directory namespace validation failed: %w", err))
				return
			}
		case _, ok := <-w.inboxWatcher.Events:
			if !ok {
				return
			}
			if err := w.authority.validateCanonical(); err != nil {
				w.fail(fmt.Errorf("retained wake directory namespace validation failed: %w", err))
				return
			}
			select {
			case w.events <- fsnotify.Event{Name: eventName, Op: fsnotify.Write}:
			default:
			}
		case err, ok := <-w.namespaceWatcher.Errors:
			if !ok {
				return
			}
			w.fail(fmt.Errorf("watch retained wake namespace: %w", err))
			return
		case err, ok := <-w.inboxWatcher.Errors:
			if !ok {
				return
			}
			w.fail(fmt.Errorf("watch retained wake inbox: %w", err))
			return
		}
	}
}

func (w *retainedWakeInboxFSNotifyWatcher) fail(err error) {
	select {
	case w.errors <- err:
	default:
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
		namespaceErr := w.namespaceWatcher.Close()
		inboxErr := w.inboxWatcher.Close()
		<-w.done
		w.closeErr = errors.Join(
			namespaceErr,
			inboxErr,
			w.inboxParent.Close(),
		)
	})
	return w.closeErr
}
