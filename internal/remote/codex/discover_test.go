package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// 611.13: discovery lists each loaded thread of a running app-server daemon
// with the manifest config needed to attach it.
func TestDiscoverListsLoadedThreads(t *testing.T) {
	dir, err := os.MkdirTemp("", "amqcxd")
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
			result := `{"userAgent":"fake"}`
			if msg.Method == "thread/loaded/list" {
				result = `{"data":["0198a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"]}`
			}
			_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":` + result + `}`))
		}
	}()
	// The socket arrives as the --codex-socket hint (codex 611.13 consult).
	cands, err := discoverer{}.Discover(context.Background(), registry.DiscoverRequest{Hints: map[string]string{HintSocket: sock}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Kind != "codex" || cands[0].Target != "codex:0198a1b2c3d47e5f8a9b0c1d2e3f4a5b" {
		t.Fatalf("candidates = %+v", cands)
	}
	var cfg map[string]string
	if err := json.Unmarshal(cands[0].Config, &cfg); err != nil || cfg["socket"] != sock || cfg["thread"] != "0198a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" {
		t.Fatalf("config = %s", cands[0].Config)
	}
}
