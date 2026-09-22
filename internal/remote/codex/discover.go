package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// defaultControlSocket is where the Codex CLI's managed app-server daemon
// listens (`codex app-server daemon version` names this path, codex-cli
// 0.155.1).
func defaultControlSocket(home string) string {
	return filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock")
}

// discoverer lists the loaded threads of the running Codex app-server
// daemon (bead 611.13). No daemon socket means no candidates; discovery
// never starts the daemon and never attaches.
type discoverer struct {
	// socket overrides the daemon socket for tests; empty = the default.
	socket string
}

// HintSocket is the DiscoverRequest hint serve fills from --codex-socket; it
// replaces the default daemon socket (codex 611.13 consult, requirement 2).
const HintSocket = "codex.socket"

func (d discoverer) Discover(_ context.Context, req registry.DiscoverRequest) ([]registry.Candidate, error) {
	sock := d.socket
	if h := req.Hints[HintSocket]; h != "" {
		sock = h
	}
	if sock == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil
		}
		sock = defaultControlSocket(home)
	}
	fi, err := os.Lstat(sock)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return nil, nil // no running daemon: nothing to discover
	}
	threads, err := LoadedThreads(sock)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Candidate, 0, len(threads))
	for _, th := range threads {
		cfg, _ := json.Marshal(map[string]string{"socket": sock, "thread": th})
		out = append(out, registry.Candidate{Kind: "codex", Target: TargetID(th), Config: cfg})
	}
	return out, nil
}

func init() {
	registry.RegisterDiscoverer("codex", discoverer{})
}
