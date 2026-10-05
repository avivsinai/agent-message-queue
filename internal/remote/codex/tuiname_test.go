package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 4ip (field, codex-cli 0.160, 2026-10-05): a Codex TUI runs on the shared
// app-server daemon. coop exec reads the daemon to see the new TUI thread in
// its directory and to confirm the name it typed: a thread from before the
// launch, in another directory, or of a subagent is not listed.
func TestNewTUIThreadsListsOnlyNewTopLevelThreadsInTheDirectory(t *testing.T) {
	d := newFakeNamingDaemon(t)
	now := time.Now()
	named := "session1/codex"
	d.add("new", d.cwd, now.Unix(), &named, nil)
	d.add("old", d.cwd, now.Unix()-600, nil, nil)
	d.add("elsewhere", t.TempDir(), now.Unix(), nil, nil)
	parent := "new"
	d.add("subagent", d.cwd, now.Unix(), nil, &parent)
	threads, err := NewTUIThreads(context.Background(), d.sock, d.cwd, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0] != (TUIThread{ID: "new", Name: named}) {
		t.Fatalf("threads = %+v", threads)
	}
}

// fakeNamingDaemon serves app-server connections that list and read
// threads.
type fakeNamingDaemon struct {
	sock, cwd string
	mu        sync.Mutex
	threads   map[string]map[string]any
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

func (d *fakeNamingDaemon) add(id, cwd string, created int64, name, parent *string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	th := map[string]any{"id": id, "cwd": cwd, "originator": "codex-tui", "threadSource": "user", "parentThreadId": nil, "createdAt": created, "name": nil}
	if name != nil {
		th["name"] = *name
	}
	if parent != nil {
		th["parentThreadId"] = *parent
	}
	d.threads[id] = th
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
		}
		raw, _ := json.Marshal(result)
		d.mu.Unlock()
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":` + string(raw) + `}`))
	}
}
