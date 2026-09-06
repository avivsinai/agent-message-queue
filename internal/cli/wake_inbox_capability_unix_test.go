//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestOpenWakeRepairInboxDirRejectsSymlinkedComponents(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(t *testing.T, agentPath, outsidePath string)
	}{
		{
			name: "intermediate inbox",
			replace: func(t *testing.T, agentPath, outsidePath string) {
				t.Helper()
				inboxPath := filepath.Join(agentPath, "inbox")
				if err := os.RemoveAll(inboxPath); err != nil {
					t.Fatalf("remove canonical inbox: %v", err)
				}
				if err := os.Symlink(outsidePath, inboxPath); err != nil {
					t.Fatalf("symlink canonical inbox: %v", err)
				}
			},
		},
		{
			name: "final new",
			replace: func(t *testing.T, agentPath, outsidePath string) {
				t.Helper()
				newPath := filepath.Join(agentPath, "inbox", "new")
				if err := os.Remove(newPath); err != nil {
					t.Fatalf("remove canonical inbox/new: %v", err)
				}
				if err := os.Symlink(filepath.Join(outsidePath, "new"), newPath); err != nil {
					t.Fatalf("symlink canonical inbox/new: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, agentDir := newWakeInboxCapabilityForTest(t)
			agentPath := fsq.AgentBase(root, "codex")
			outsidePath := filepath.Join(t.TempDir(), "outside-inbox")
			if err := os.MkdirAll(filepath.Join(outsidePath, "new"), 0o700); err != nil {
				t.Fatal(err)
			}
			test.replace(t, agentPath, outsidePath)

			inboxDir, err := openWakeRepairInboxDir(agentDir)
			if inboxDir != nil {
				_ = inboxDir.Close()
				t.Fatal("symlinked mailbox returned a retained inbox capability")
			}
			if err == nil {
				t.Fatal("symlinked mailbox was accepted")
			}
		})
	}
}

func newWakeInboxCapabilityForTest(t *testing.T) (string, *wakeAgentDir) {
	t.Helper()
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	return root, agentDir
}
