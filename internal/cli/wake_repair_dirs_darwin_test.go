//go:build darwin

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDarwinRetainedWakeInboxWatcherFailsOnRootLoss(t *testing.T) {
	for _, test := range []struct {
		name string
		lose func(*testing.T, string)
	}{
		{
			name: "rename",
			lose: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Rename(path, filepath.Join(filepath.Dir(path), "new-detached")); err != nil {
					t.Fatalf("rename retained inbox: %v", err)
				}
			},
		},
		{
			name: "delete",
			lose: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("delete retained inbox: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			watcher, inboxPath := newDarwinRetainedWakeInboxWatcherForTest(t)
			test.lose(t, inboxPath)
			select {
			case err, ok := <-watcher.Errors():
				if !ok || err == nil || !strings.Contains(err.Error(), "renamed or deleted") {
					t.Fatalf("root-loss error = %v ok=%v", err, ok)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("retained watcher continued blind after inbox root loss")
			}
		})
	}
}

func newDarwinRetainedWakeInboxWatcherForTest(t *testing.T) (wakeEventWatcher, string) {
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
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inboxDir.Close() })
	watcher, err := inboxDir.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	return watcher, fsq.AgentInboxNew(root, "codex")
}
