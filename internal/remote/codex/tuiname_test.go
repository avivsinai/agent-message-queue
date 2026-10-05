package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 4ip (field, codex-cli 0.160, 2026-10-05): a Codex TUI runs on the shared
// app-server daemon, so coop exec never found its thread and the session
// never showed <session>/<handle>. The launch's thread is named through the
// daemon, once it is the sole new candidate on two polls in a row; a thread
// loaded before the launch is left alone.
func TestSpawnedTUINamerNamesOnlyTheNewThread(t *testing.T) {
	d := newFakeNamingDaemon(t)
	now := time.Now()
	d.add("old", now.Unix()-600, nil)
	d.add("new", now.Unix(), nil)
	n := NewSpawnedTUINamer(SpawnedTUI{Baseline: map[string]bool{"old": true}, Cwd: d.cwd, Since: now}, "session1/codex", time.Minute)
	if err := n.Poll(context.Background(), d.sock); !errors.Is(err, ErrTUIThreadNotLoaded) {
		t.Fatalf("first poll = %v, want not loaded until the candidate is stable", err)
	}
	if err := n.Poll(context.Background(), d.sock); err != nil {
		t.Fatal(err)
	}
	if d.name("new") != "session1/codex" || d.name("old") != "" {
		t.Fatalf("names: new=%q old=%q", d.name("new"), d.name("old"))
	}
}

// Review of #950, P1 and P2s: a rejected rename is an error, never success;
// a thread another launch named is not this launch's; the window closes.
func TestSpawnedTUINamerRefusals(t *testing.T) {
	now := time.Now()
	other := "other/codex"
	for _, tc := range []struct {
		name    string
		window  time.Duration
		named   *string
		rejects bool
		want    func(error) bool
	}{
		{name: "rejected rename", window: time.Minute, rejects: true, want: func(err error) bool { return err != nil && !errors.Is(err, ErrTUIThreadNotLoaded) }},
		{name: "named by another launch", window: time.Minute, named: &other, want: func(err error) bool { return errors.Is(err, ErrTUIThreadNotLoaded) }},
		{name: "window closed", window: -time.Second, want: func(err error) bool { return errors.Is(err, ErrTUIThreadWindowClosed) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeNamingDaemon(t)
			d.rejectSet = tc.rejects
			d.add("new", now.Unix(), tc.named)
			n := NewSpawnedTUINamer(SpawnedTUI{Cwd: d.cwd, Since: now}, "session1/codex", tc.window)
			_ = n.Poll(context.Background(), d.sock)
			if err := n.Poll(context.Background(), d.sock); !tc.want(err) {
				t.Fatalf("second poll = %v", err)
			}
			if tc.named != nil && d.name("new") != *tc.named {
				t.Fatalf("renamed another launch's thread to %q", d.name("new"))
			}
		})
	}
}

// fakeNamingDaemon serves app-server connections that list, read, and
// rename threads in one directory.
type fakeNamingDaemon struct {
	sock, cwd string
	mu        sync.Mutex
	threads   map[string]map[string]any
	rejectSet bool
}

func newFakeNamingDaemon(t *testing.T) *fakeNamingDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "amqcxn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := &fakeNamingDaemon{sock: filepath.Join(dir, "d.sock"), cwd: t.TempDir(), threads: map[string]map[string]any{}}
	l, err := net.Listen("unix", d.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go d.serve(conn)
		}
	}()
	return d
}

func (d *fakeNamingDaemon) add(id string, created int64, name *string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	th := map[string]any{"id": id, "cwd": d.cwd, "originator": "codex-tui", "threadSource": "user", "parentThreadId": nil, "createdAt": created, "name": nil}
	if name != nil {
		th["name"] = *name
	}
	d.threads[id] = th
}

func (d *fakeNamingDaemon) name(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, _ := d.threads[id]["name"].(string)
	return s
}

func (d *fakeNamingDaemon) serve(conn net.Conn) {
	ws, err := acceptServerWS(conn)
	if err != nil {
		return
	}
	for {
		payload, err := ws.readText()
		if err != nil {
			return
		}
		var msg rpcMessage
		if json.Unmarshal(payload, &msg) != nil || msg.ID == nil {
			continue
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Name     string `json:"name"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		var result any = map[string]any{}
		d.mu.Lock()
		switch msg.Method {
		case "thread/loaded/list":
			ids := []string{}
			for id := range d.threads {
				ids = append(ids, id)
			}
			result = map[string]any{"data": ids}
		case "thread/read":
			result = map[string]any{"thread": d.threads[p.ThreadID]}
		case "thread/name/set":
			if d.rejectSet {
				d.mu.Unlock()
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"error":{"code":-32600,"message":"rejected"}}`))
				continue
			}
			d.threads[p.ThreadID]["name"] = p.Name
		}
		raw, _ := json.Marshal(result)
		d.mu.Unlock()
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":` + string(raw) + `}`))
	}
}
