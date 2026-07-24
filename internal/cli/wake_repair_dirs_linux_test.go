//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

func TestLinuxRetainedWakeInboxWatcherFailsOnRootLoss(t *testing.T) {
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
			watcher, inboxPath := newLinuxRetainedWakeInboxWatcherForTest(t)
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

func TestLinuxRetainedWakeInboxWatcherNormalizesCreateToMessageScanTrigger(t *testing.T) {
	watcher, inboxPath := newLinuxRetainedWakeInboxWatcherForTest(t)
	if err := os.WriteFile(filepath.Join(inboxPath, "delivered.md"), []byte("message"), 0o600); err != nil {
		t.Fatalf("create retained inbox message: %v", err)
	}
	select {
	case event, ok := <-watcher.Events():
		if !ok {
			t.Fatal("retained watcher closed before forwarding message trigger")
		}
		if !strings.HasSuffix(event.Name, ".md") || event.Op&fsnotify.Write == 0 {
			t.Fatalf("normalized retained event = %#v, want synthetic .md write", event)
		}
	case err := <-watcher.Errors():
		t.Fatalf("retained watcher failed on message create: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("retained watcher did not forward a message scan trigger")
	}
}

func newLinuxRetainedWakeInboxWatcherForTest(t *testing.T) (wakeEventWatcher, string) {
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
