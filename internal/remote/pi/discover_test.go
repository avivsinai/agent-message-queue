package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// A live pi chat under the root is listed with its pi session id and a
// manifest entry that attaches it (agent-message-queue-9dx.7).
func TestDiscoverListsLiveChatWithItsSessionID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "chat-1", "extensions", "pi-bridge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stampLivenessRevisionAt(t, dir, fixedNow, ApproveBridgeRevision)

	cands, err := discoverer{now: func() time.Time { return fixedNow }}.Discover(context.Background(), registry.DiscoverRequest{Root: root})
	if err != nil || len(cands) != 1 {
		t.Fatalf("Discover = %+v, %v; want one candidate", cands, err)
	}
	c := cands[0]
	if c.Kind != "pi" || c.Target != "pi:chat-1" || c.NativeSession != "sess-1" || string(c.Config) != `{"handle":"chat-1"}` {
		t.Fatalf("candidate = %+v", c)
	}
	att, err := Factory(context.Background(), registry.FactoryConfig{Root: root, Target: c.Target, Config: c.Config})
	if err != nil {
		t.Fatal(err)
	}
	a := att.(*Attachment)
	a.SetNow(func() time.Time { return fixedNow })
	if got := a.NativeSessionID(); got != c.NativeSession {
		t.Fatalf("attached native session = %q, want %q", got, c.NativeSession)
	}
}
