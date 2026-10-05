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

// ErrAmbiguousTUIThread means more than one new Codex TUI thread matches a
// launch, so none of them is named.
var ErrAmbiguousTUIThread = errors.New("more than one new Codex TUI thread was started in this directory")

// SpawnedTUI identifies the thread a Codex TUI launch creates on the shared
// daemon: a top-level user thread of the codex-tui originator, in Cwd,
// created at or after Since, and not loaded before the launch (Baseline).
type SpawnedTUI struct {
	Baseline map[string]bool
	Cwd      string
	Since    time.Time
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

// NameSpawnedTUIThread gives the launch's thread name through the daemon the
// TUI is connected to, so the TUI shows it at once. A thread that already
// has a name keeps it: the owner renamed it, and before the first turn Codex
// has not named it. found is false while the thread is not loaded yet.
func NameSpawnedTUIThread(ctx context.Context, sock string, spawn SpawnedTUI, name string) (found bool, err error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return false, err
	}
	defer func() { _ = client.Close() }()
	if err := client.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "amq", "version": Version}}, nil); err != nil {
		return false, fmt.Errorf("initialize: %w", err)
	}
	var loaded struct {
		Data      []string `json:"data"`
		ThreadIDs []string `json:"threadIds"`
	}
	if err := client.Call(ctx, "thread/loaded/list", map[string]any{}, &loaded); err != nil {
		return false, fmt.Errorf("list loaded threads: %w", err)
	}
	ids := loaded.Data
	if len(ids) == 0 {
		ids = loaded.ThreadIDs
	}
	cwd := realPath(spawn.Cwd)
	var match *tuiThread
	for _, id := range ids {
		if spawn.Baseline[id] {
			continue
		}
		th, err := readTUIThread(ctx, client, id)
		if err != nil {
			return false, err
		}
		if th.Originator != "codex-tui" || th.ThreadSource != "user" || th.ParentThreadID != nil ||
			th.CreatedAt < spawn.Since.Unix() || realPath(th.Cwd) != cwd {
			continue
		}
		if match != nil {
			return false, ErrAmbiguousTUIThread
		}
		match = &th
	}
	if match == nil {
		return false, nil
	}
	if match.Name != nil && strings.TrimSpace(*match.Name) != "" {
		return true, nil
	}
	if err := client.Call(ctx, "thread/name/set", map[string]string{"threadId": match.ID, "name": name}, nil); err != nil {
		return true, fmt.Errorf("set thread name: %w", err)
	}
	th, err := readTUIThread(ctx, client, match.ID)
	if err != nil {
		return true, err
	}
	if th.Name == nil || *th.Name != name {
		return true, errors.New("the daemon did not confirm the thread name")
	}
	return true, nil
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
