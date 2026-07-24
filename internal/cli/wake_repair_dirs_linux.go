//go:build linux

package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type retainedWakeInboxFSNotifyWatcher struct {
	agentWatcher *fsnotify.Watcher
	inboxWatcher *fsnotify.Watcher
	authority    retainedWakeDirectoryAuthority
	events       chan fsnotify.Event
	errors       chan error
	done         chan struct{}
	close        sync.Once
	closeErr     error
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
	agentWatcher, err := newRetainedWakeFSNotifyWatcher(
		agentFD,
		"agent",
	)
	if err != nil {
		return nil, err
	}
	inboxWatcher, err := newRetainedWakeFSNotifyWatcher(
		inboxFD,
		"inbox",
	)
	if err != nil {
		_ = agentWatcher.Close()
		return nil, err
	}
	if err := authority.validateCanonical(); err != nil {
		closeErr := errors.Join(inboxWatcher.Close(), agentWatcher.Close())
		return nil, errors.Join(
			fmt.Errorf("validate retained wake directories after watch registration: %w", err),
			closeErr,
		)
	}
	retained := &retainedWakeInboxFSNotifyWatcher{
		agentWatcher: agentWatcher,
		inboxWatcher: inboxWatcher,
		authority:    authority,
		events:       make(chan fsnotify.Event, 1),
		errors:       make(chan error, 1),
		done:         make(chan struct{}),
	}
	go retained.run()
	return retained, nil
}

func newRetainedWakeFSNotifyWatcher(
	fd int,
	label string,
) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create retained wake %s watcher: %w", label, err)
	}
	descriptorPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	if err := watcher.Add(descriptorPath); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("watch retained wake %s descriptor: %w", label, err)
	}
	return watcher, nil
}

func (w *retainedWakeInboxFSNotifyWatcher) run() {
	defer close(w.done)
	defer close(w.events)
	defer close(w.errors)
	eventName := filepath.Join(w.authority.inboxPath, "retained-inbox-event.md")
	for {
		select {
		case event, ok := <-w.agentWatcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
				w.fail(fmt.Errorf("retained wake agent directory was renamed or deleted"))
				return
			}
			if err := w.authority.validateCanonical(); err != nil {
				w.fail(fmt.Errorf("retained wake directory namespace validation failed: %w", err))
				return
			}
		case event, ok := <-w.inboxWatcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
				w.fail(fmt.Errorf("retained wake inbox directory was renamed or deleted"))
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
		case err, ok := <-w.agentWatcher.Errors:
			if !ok {
				return
			}
			w.fail(fmt.Errorf("watch retained wake agent: %w", err))
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
		w.closeErr = errors.Join(
			w.inboxWatcher.Close(),
			w.agentWatcher.Close(),
		)
		<-w.done
	})
	return w.closeErr
}
