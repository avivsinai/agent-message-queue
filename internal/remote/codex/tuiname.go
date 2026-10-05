package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ControlSocket is the managed app-server daemon's control socket, checked
// the way discovery checks it. An error wrapping os.ErrNotExist means no
// daemon is running.
func ControlSocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	sock := defaultControlSocket(home)
	return sock, checkControlSocket(sock)
}

// The outcomes of SpawnedTUINamer.Poll other than nil (named) and a hard
// failure after the launch's thread was found.
var (
	// ErrTUIThreadNotLoaded: the launch's thread is not known yet; poll again.
	ErrTUIThreadNotLoaded = errors.New("the Codex TUI thread is not loaded yet")
	// ErrTUIThreadWindowClosed: the window to find the thread has passed.
	ErrTUIThreadWindowClosed = errors.New("the Codex TUI thread did not appear on the daemon in time")
	// ErrAmbiguousTUIThread: more than one new thread matches the launch.
	ErrAmbiguousTUIThread = errors.New("more than one new Codex TUI thread was started in this directory")
)

// SpawnedTUI identifies the thread a Codex TUI launch creates on the shared
// daemon: a top-level user thread of the codex-tui originator, in Cwd,
// created at or after Since, and not loaded before the launch (Baseline).
type SpawnedTUI struct {
	Baseline map[string]bool
	Cwd      string
	Since    time.Time
}

// SpawnedTUINamer names one launch's thread through the daemon the TUI is
// connected to, so the TUI shows the name at once, before the first turn
// and so before Codex names the thread itself.
type SpawnedTUINamer struct {
	spawn    SpawnedTUI
	name     string
	deadline time.Time
	seen     string // the sole candidate of the previous poll
}

// NewSpawnedTUINamer starts the window to find the launch's thread.
func NewSpawnedTUINamer(spawn SpawnedTUI, name string, window time.Duration) *SpawnedTUINamer {
	return &SpawnedTUINamer{spawn: spawn, name: name, deadline: time.Now().Add(window)}
}

type tuiThread struct {
	ID             string  `json:"id"`
	Cwd            string  `json:"cwd"`
	Originator     string  `json:"originator"`
	ThreadSource   string  `json:"threadSource"`
	ParentThreadID *string `json:"parentThreadId"`
	CreatedAt      int64   `json:"createdAt"`
	Name           *string `json:"name"`
}

func (t tuiThread) name() string {
	if t.Name == nil {
		return ""
	}
	return strings.TrimSpace(*t.Name)
}

// Poll checks the daemon once. It returns nil when the launch's thread
// carries the name or one given meanwhile, ErrTUIThreadNotLoaded to poll again, or another error.
// A thread is named only when it is the sole candidate on two polls in a
// row, and a thread that has any name is never renamed (review of #950).
func (n *SpawnedTUINamer) Poll(ctx context.Context, sock string) error {
	if time.Now().After(n.deadline) {
		return ErrTUIThreadWindowClosed
	}
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTUIThreadNotLoaded, err)
	}
	defer func() { _ = client.Close() }()
	ids, err := loadedThreads(ctx, client)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTUIThreadNotLoaded, err)
	}
	cwd := realPath(n.spawn.Cwd)
	var candidates []tuiThread
	for _, id := range ids {
		if n.spawn.Baseline[id] {
			continue
		}
		th, err := readTUIThread(ctx, client, id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrTUIThreadNotLoaded, err)
		}
		if th.Originator != "codex-tui" || th.ThreadSource != "user" || th.ParentThreadID != nil ||
			th.CreatedAt < n.spawn.Since.Unix() || realPath(th.Cwd) != cwd {
			continue
		}
		// Before its first turn only this launch names its thread, so a
		// thread named otherwise belongs to another launch, unless it is the
		// candidate this launch already saw: then it was named meanwhile and
		// keeps that name, and the search ends (review of #950 r2).
		if th.name() != "" && th.name() != n.name {
			if th.ID == n.seen {
				return nil
			}
			continue
		}
		candidates = append(candidates, th)
	}
	switch {
	case len(candidates) > 1:
		return ErrAmbiguousTUIThread
	case len(candidates) == 0:
		n.seen = ""
		return ErrTUIThreadNotLoaded
	}
	th := candidates[0]
	if th.name() == n.name {
		return nil
	}
	if n.seen != th.ID {
		n.seen = th.ID // a TUI started alongside may load its thread first
		return ErrTUIThreadNotLoaded
	}
	if err := client.Call(ctx, "thread/name/set", map[string]string{"threadId": th.ID, "name": n.name}, nil); err != nil {
		return fmt.Errorf("set thread name: %w", err)
	}
	th, err = readTUIThread(ctx, client, th.ID)
	if err != nil {
		return err
	}
	if th.name() != n.name {
		return errors.New("the daemon did not confirm the thread name")
	}
	return nil
}

// LoadedThreadIDs lists the daemon's loaded threads within ctx.
func LoadedThreadIDs(ctx context.Context, sock string) ([]string, error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	return loadedThreads(ctx, client)
}

func loadedThreads(ctx context.Context, client *Client) ([]string, error) {
	if err := client.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "amq", "version": Version}}, nil); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var loaded struct {
		Data      []string `json:"data"`
		ThreadIDs []string `json:"threadIds"`
	}
	if err := client.Call(ctx, "thread/loaded/list", map[string]any{}, &loaded); err != nil {
		return nil, fmt.Errorf("list loaded threads: %w", err)
	}
	if len(loaded.Data) > 0 {
		return loaded.Data, nil
	}
	return loaded.ThreadIDs, nil
}

func readTUIThread(ctx context.Context, client *Client, id string) (tuiThread, error) {
	var res struct {
		Thread tuiThread `json:"thread"`
	}
	if err := client.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &res); err != nil {
		return tuiThread{}, fmt.Errorf("read thread %s: %w", id, err)
	}
	if res.Thread.ID != id {
		return tuiThread{}, fmt.Errorf("read thread %s: the daemon returned thread %q", id, res.Thread.ID)
	}
	return res.Thread, nil
}

// realPath resolves symlinks so /tmp and /private/tmp compare equal.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
