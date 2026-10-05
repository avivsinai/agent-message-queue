package codex

import (
	"context"
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

// TUIThread is a top-level user thread a Codex TUI started on the daemon.
type TUIThread struct {
	ID   string
	Name string
}

// NewTUIThreads lists, read-only, the TUI threads in cwd created at or after
// since. The daemon does not say which process owns a thread, so a caller
// uses this only to see that a TUI is up and to confirm a name, never to
// choose a thread to change (review of #950).
func NewTUIThreads(ctx context.Context, sock, cwd string, since time.Time) ([]TUIThread, error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
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
	ids := loaded.Data
	if len(ids) == 0 {
		ids = loaded.ThreadIDs
	}
	cwd = realPath(cwd)
	var out []TUIThread
	for _, id := range ids {
		var res struct {
			Thread struct {
				ID             string  `json:"id"`
				Cwd            string  `json:"cwd"`
				Originator     string  `json:"originator"`
				ThreadSource   string  `json:"threadSource"`
				ParentThreadID *string `json:"parentThreadId"`
				CreatedAt      int64   `json:"createdAt"`
				Name           *string `json:"name"`
			} `json:"thread"`
		}
		if err := client.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &res); err != nil {
			return nil, fmt.Errorf("read thread %s: %w", id, err)
		}
		th := res.Thread
		if th.ID != id || th.Originator != "codex-tui" || th.ThreadSource != "user" || th.ParentThreadID != nil ||
			th.CreatedAt < since.Unix() || realPath(th.Cwd) != cwd {
			continue
		}
		name := ""
		if th.Name != nil {
			name = strings.TrimSpace(*th.Name)
		}
		out = append(out, TUIThread{ID: th.ID, Name: name})
	}
	return out, nil
}

// realPath resolves symlinks so /tmp and /private/tmp compare equal.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
