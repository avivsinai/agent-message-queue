//go:build unix

package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// 611.51 (field, codex-cli 0.160): the default control socket is a symlink
// to the daemon's socket under a per-user tmp dir. Discovery refused it as
// "not a unix socket (mode Lrwxr-xr-x)"; it must follow the link to a socket
// this user owns and keep the link path in the attach config.
func TestDiscoverFollowsSymlinkedDefaultSocket(t *testing.T) {
	target := fakeLoadedDaemon(t, "daemon")
	link := filepath.Join(filepath.Dir(filepath.Dir(target)), "control.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cands, err := discoverer{socket: link}.Discover(context.Background(), registry.DiscoverRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v", cands)
	}
	var cfg map[string]string
	if err := json.Unmarshal(cands[0].Config, &cfg); err != nil || cfg["socket"] != link {
		t.Fatalf("config = %s", cands[0].Config)
	}
}
