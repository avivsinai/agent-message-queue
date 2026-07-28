package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMoveNewToCurPostRenameSyncFailureReportsCommittedClaim(t *testing.T) {
	const (
		agent    = "alice"
		filename = "claimed.md"
	)

	for _, failure := range []struct {
		name string
		dir  string
	}{
		{name: "inbox new", dir: filepath.Join("agents", agent, "inbox", "new")},
		{name: "inbox cur", dir: filepath.Join("agents", agent, "inbox", "cur")},
	} {
		t.Run(failure.name, func(t *testing.T) {
			base := t.TempDir()
			if err := EnsureAgentDirs(base, agent); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}
			newPath := filepath.Join(AgentInboxNew(base, agent), filename)
			if err := os.WriteFile(newPath, []byte("payload"), 0o600); err != nil {
				t.Fatalf("write inbox message: %v", err)
			}

			root := openDeliveryRootForTest(t, base)
			injectedErr := errors.New("injected post-rename sync failure")
			syncCalls := make(map[string]int)
			root.syncDirForTest = func(dir string) error {
				syncCalls[dir]++
				if dir == failure.dir {
					return injectedErr
				}
				return root.syncDirPlatform(dir)
			}

			err := MoveNewToCur(root, agent, filename)
			var committed *CommittedDurabilityError
			if !errors.As(err, &committed) {
				t.Fatalf("MoveNewToCur error = %T %v, want CommittedDurabilityError", err, err)
			}
			if !errors.Is(err, injectedErr) {
				t.Fatalf("MoveNewToCur error = %v, want injected sync failure", err)
			}

			curPath := filepath.Join(AgentInboxCur(base, agent), filename)
			if committed.FinalPath != curPath || committed.Recipient != agent {
				t.Fatalf(
					"committed claim = (%q, %q), want (%q, %q)",
					committed.FinalPath,
					committed.Recipient,
					curPath,
					agent,
				)
			}
			if got := syncCalls[filepath.Join("agents", agent, "inbox", "new")]; got != 1 {
				t.Fatalf("inbox/new sync calls = %d, want 1", got)
			}
			if got := syncCalls[filepath.Join("agents", agent, "inbox", "cur")]; got != 1 {
				t.Fatalf("inbox/cur sync calls = %d, want 1", got)
			}
			if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
				t.Fatalf("claimed message remains in inbox/new: %v", statErr)
			}
			if data, readErr := os.ReadFile(curPath); readErr != nil || string(data) != "payload" {
				t.Fatalf("claimed message in inbox/cur = %q, err=%v; want payload", data, readErr)
			}
		})
	}
}
