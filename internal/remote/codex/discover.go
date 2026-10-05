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
	if err := checkControlSocket(sock); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist) && !explicit:
			return nil, nil // no daemon at the default path: nothing to discover
		case explicit:
			return nil, fmt.Errorf("%w: --codex-socket: %v", registry.ErrBadHint, err)
		default:
			return nil, err
		}
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

// checkControlSocket accepts sock when it is a unix socket, or a symlink to a
// unix socket owned by the current user. codex-cli 0.160 makes the default
// path a symlink to /private/tmp/codex-daemon-<uid>/<hash> (bead 611.51); the
// target sits under a shared directory, so a symlink is followed only to a
// socket this user owns. A dangling link reports os.ErrNotExist.
func checkControlSocket(sock string) error {
	fi, err := os.Lstat(sock)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s is not a unix socket (mode %s)", sock, fi.Mode())
		}
		return nil
	}
	target, err := os.Stat(sock)
	if err != nil {
		return err
	}
	if target.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s links to a non-socket (mode %s)", sock, target.Mode())
	}
	if !ownedByCurrentUser(target) {
		return fmt.Errorf("%s links to a socket the current user does not own", sock)
	}
	return nil
}

func init() {
	registry.RegisterDiscoverer("codex", discoverer{})
}
