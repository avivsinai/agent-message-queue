package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	sock, explicit := d.socket, false
	if h := req.Hints[HintSocket]; h != "" {
		sock, explicit = h, true
	}
	if sock == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home: %w", err)
		}
		sock = defaultControlSocket(home)
	}
	fi, err := os.Lstat(sock)
	switch {
	case err != nil && errors.Is(err, os.ErrNotExist) && !explicit:
		return nil, nil // no daemon at the default path: nothing to discover
	case err != nil && explicit:
		return nil, fmt.Errorf("%w: --codex-socket %s: %v", registry.ErrBadHint, sock, err)
	case err != nil:
		return nil, fmt.Errorf("stat %s: %w", sock, err)
	case fi.Mode()&os.ModeSocket == 0 && explicit:
		return nil, fmt.Errorf("%w: --codex-socket %s is not a unix socket (mode %s)", registry.ErrBadHint, sock, fi.Mode())
	case fi.Mode()&os.ModeSocket == 0:
		return nil, fmt.Errorf("%s is not a unix socket (mode %s)", sock, fi.Mode())
	}
	threads, err := LoadedThreads(sock)
	if err != nil {
		// A stale socket (daemon gone) fails here; it becomes a diagnostic
		// for this discoverer only, never a failure of discovery as a whole.
		return nil, fmt.Errorf("list loaded threads on %s: %w", sock, err)
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
