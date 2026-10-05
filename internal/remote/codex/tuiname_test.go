package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 4ip (field, codex-cli 0.160, 2026-10-05): a Codex TUI runs on the shared
// app-server daemon, so coop exec never found its thread and the session
// never showed <session>/<handle>. The launch's thread is named through the
// daemon; a thread loaded before the launch is left alone.
func TestNameSpawnedTUIThreadNamesOnlyTheNewThread(t *testing.T) {
	cwd := t.TempDir()
	now := time.Now()
	threads := map[string]map[string]any{
		"old": {"id": "old", "cwd": cwd, "originator": "codex-tui", "threadSource": "user", "parentThreadId": nil, "createdAt": now.Unix() - 600, "name": nil},
		"new": {"id": "new", "cwd": cwd, "originator": "codex-tui", "threadSource": "user", "parentThreadId": nil, "createdAt": now.Unix(), "name": nil},
	}
	sock := fakeNamingDaemon(t, threads)
	found, err := NameSpawnedTUIThread(context.Background(), sock, SpawnedTUI{Baseline: map[string]bool{"old": true}, Cwd: cwd, Since: now}, "session1/codex")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if threads["new"]["name"] != "session1/codex" || threads["old"]["name"] != nil {
		t.Fatalf("names: new=%v old=%v", threads["new"]["name"], threads["old"]["name"])
	}
}

// fakeNamingDaemon serves one app-server connection that lists, reads, and
// renames threads. Each thread holds its thread/read fields.
func fakeNamingDaemon(t *testing.T, threads map[string]map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "amqcxn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
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
			switch msg.Method {
			case "thread/loaded/list":
				ids := []string{}
				for id := range threads {
					ids = append(ids, id)
				}
				result = map[string]any{"data": ids}
			case "thread/read":
				result = map[string]any{"thread": threads[p.ThreadID]}
			case "thread/name/set":
				threads[p.ThreadID]["name"] = p.Name
			}
			raw, _ := json.Marshal(result)
			_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":` + string(raw) + `}`))
		}
	}()
	return sock
}
