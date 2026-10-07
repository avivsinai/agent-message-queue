package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 4ip (field, codex-cli 0.160, 2026-10-05): a Codex TUI on the shared
// app-server daemon never got the session name. coop exec now creates the
// thread on the daemon, names it, persists it, and resumes it.
func TestStartNamedThreadCreatesNamesAndPersistsTheThread(t *testing.T) {
	d := newFakeNamingDaemon(t)
	id, err := StartNamedThread(context.Background(), d.sock, "/work", "session1/codex")
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if id != "t1" || d.cwd != "/work" || d.name != "session1/codex" || !d.persisted {
		t.Fatalf("id=%q cwd=%q name=%q persisted=%v", id, d.cwd, d.name, d.persisted)
	}
}

// fakeNamingDaemon serves app-server connections that start, name, and
// persist one thread.
type fakeNamingDaemon struct {
	sock      string
	mu        sync.Mutex
	cwd, name string
	persisted bool
}

func newFakeNamingDaemon(t *testing.T) *fakeNamingDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "amqcxn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := &fakeNamingDaemon{sock: filepath.Join(dir, "d.sock")}
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
			ThreadID string            `json:"threadId"`
			Cwd      string            `json:"cwd"`
			Name     string            `json:"name"`
			Items    []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		var result any = map[string]any{}
		d.mu.Lock()
		switch msg.Method {
		case "thread/start":
			d.cwd = p.Cwd
			result = map[string]any{"thread": map[string]any{"id": "t1"}}
		case "thread/name/set":
			if p.ThreadID == "t1" {
				d.name = p.Name
			}
		case "thread/inject_items":
			d.persisted = p.ThreadID == "t1" && len(p.Items) == 1
		}
		raw, _ := json.Marshal(result)
		d.mu.Unlock()
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":` + string(raw) + `}`))
	}
}
