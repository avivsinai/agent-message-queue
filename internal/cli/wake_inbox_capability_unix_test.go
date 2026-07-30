//go:build darwin || linux

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

func TestOpenWakeRepairInboxDirValidatesEveryComponentMode(t *testing.T) {
	for _, test := range []struct {
		name string
		path func(string) string
	}{
		{
			name: "intermediate inbox",
			path: func(agentPath string) string {
				return filepath.Join(agentPath, "inbox")
			},
		},
		{
			name: "final new",
			path: func(agentPath string) string {
				return filepath.Join(agentPath, "inbox", "new")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, agentDir := newWakeInboxCapabilityForTest(t)
			if err := os.Chmod(test.path(fsq.AgentBase(root, "codex")), 0o770); err != nil {
				t.Fatal(err)
			}

			inboxDir, err := openWakeRepairInboxDir(agentDir)
			if inboxDir != nil {
				_ = inboxDir.Close()
				t.Fatal("group-writable mailbox returned a retained inbox capability")
			}
			if err == nil || !strings.Contains(err.Error(), "group/world-writable") {
				t.Fatalf("group-writable mailbox error = %v", err)
			}
		})
	}
}

func TestOpenWatchedWakeInboxDirRejectsCanonicalAgentReplacement(t *testing.T) {
	root, agentDir := newWakeInboxCapabilityForTest(t)
	agentPath := fsq.AgentBase(root, "codex")
	if err := os.Rename(agentPath, agentPath+".detached"); err != nil {
		t.Fatalf("detach retained agent: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("create replacement agent: %v", err)
	}

	inboxDir, watcher, err := openWatchedWakeInboxDir(agentDir)
	if inboxDir != nil || watcher != nil {
		if watcher != nil {
			_ = watcher.Close()
		}
		if inboxDir != nil {
			_ = inboxDir.Close()
		}
		t.Fatal("canonical agent replacement returned a watched inbox capability")
	}
	if err == nil || !strings.Contains(err.Error(), "agent directory no longer matches retained authority") {
		t.Fatalf("canonical agent replacement error = %v", err)
	}
}

func TestOpenWatchedWakeInboxDirReopensReplacementUnderPinnedAgent(t *testing.T) {
	root, agentDir := newWakeInboxCapabilityForTest(t)
	inboxPath := fsq.AgentInboxNew(root, "codex")
	if err := os.Rename(inboxPath, inboxPath+".detached"); err != nil {
		t.Fatalf("detach old inbox/new: %v", err)
	}
	if err := os.Mkdir(inboxPath, 0o700); err != nil {
		t.Fatalf("create replacement inbox/new: %v", err)
	}

	inboxDir, watcher, err := openWatchedWakeInboxDir(agentDir)
	if err != nil {
		t.Fatalf("open watched replacement inbox: %v", err)
	}
	t.Cleanup(func() {
		_ = watcher.Close()
		_ = inboxDir.Close()
	})

	messagePath := filepath.Join(inboxPath, "after-rearm.md")
	if err := os.WriteFile(messagePath, []byte("message"), 0o600); err != nil {
		t.Fatalf("write replacement inbox message: %v", err)
	}
	select {
	case event, ok := <-watcher.Events():
		if !ok {
			t.Fatal("replacement watcher closed before observing message")
		}
		want := fsnotify.Event{
			Name: filepath.Join(inboxPath, "retained-inbox-event.md"),
			Op:   fsnotify.Write,
		}
		if event != want {
			t.Fatalf("replacement watcher event = %#v, want %#v", event, want)
		}
	case err, ok := <-watcher.Errors():
		t.Fatalf("replacement watcher failed: %v ok=%v", err, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("replacement watcher did not observe message")
	}
}

func TestWakeInboxCapabilityBaselineOperationsStayOnRetainedInode(t *testing.T) {
	root, agentDir := newWakeInboxCapabilityForTest(t)
	inboxPath := fsq.AgentInboxNew(root, "codex")
	messagePath := filepath.Join(inboxPath, "retained.md")
	if err := os.WriteFile(messagePath, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inboxDir.Close() })

	detachedPath := inboxPath + ".detached"
	if err := os.Rename(inboxPath, detachedPath); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outsidePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, inboxPath); err != nil {
		t.Fatal(err)
	}

	snapshot, err := inboxDir.SnapshotMessageIdentities()
	if err != nil {
		t.Fatalf("snapshot retained inbox: %v", err)
	}
	if _, ok := snapshot["retained.md"]; !ok {
		t.Fatalf("retained snapshot = %#v, want retained.md", snapshot)
	}

	marker, err := inboxDir.CreateBaselineBarrier()
	if err != nil {
		t.Fatalf("create retained barrier: %v", err)
	}
	if _, err := os.Stat(filepath.Join(detachedPath, marker)); err != nil {
		t.Fatalf("barrier missing from detached retained inode: %v", err)
	}
	outsideSentinel := filepath.Join(outsidePath, marker)
	if err := os.WriteFile(outsideSentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inboxDir.UnlinkBaselineBarrier(marker); err != nil {
		t.Fatalf("unlink retained barrier: %v", err)
	}
	if _, err := os.Stat(filepath.Join(detachedPath, marker)); !os.IsNotExist(err) {
		t.Fatalf("retained barrier survived unlink: %v", err)
	}
	if got, err := os.ReadFile(outsideSentinel); err != nil || string(got) != "outside" {
		t.Fatalf("outside same-name sentinel changed: data=%q err=%v", got, err)
	}
	if err := inboxDir.ValidateCanonical(); err == nil {
		t.Fatal("detached inbox capability passed canonical validation")
	}
}

func TestPrepareWakeBaselineRejectsInboxReplacementDuringSnapshot(t *testing.T) {
	root, agentDir := newWakeInboxCapabilityForTest(t)
	inboxPath := fsq.AgentInboxNew(root, "codex")
	if err := os.WriteFile(filepath.Join(inboxPath, "existing.md"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inboxDir.Close() })
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	if err := watcher.Add(inboxPath); err != nil {
		t.Fatal(err)
	}

	detachedPath := inboxPath + ".detached"
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outsidePath, 0o700); err != nil {
		t.Fatal(err)
	}
	originalSnapshot := snapshotWakeRetainedFileInfo
	swapped := false
	snapshotWakeRetainedFileInfo = func(
		inbox *wakeInboxDir,
		name string,
	) (os.FileInfo, error) {
		if !swapped {
			swapped = true
			if err := os.Rename(inboxPath, detachedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outsidePath, inboxPath); err != nil {
				t.Fatal(err)
			}
		}
		return originalSnapshot(inbox, name)
	}
	t.Cleanup(func() { snapshotWakeRetainedFileInfo = originalSnapshot })

	cfg := wakeConfig{
		root:              root,
		me:                "codex",
		baselineRequested: true,
		retainedInbox:     inboxDir,
	}
	err = prepareWakeBaselineEvents(
		&cfg,
		watcher.Events,
		watcher.Errors,
		inboxPath,
	)
	if err == nil || !strings.Contains(err.Error(), "after baseline snapshot") {
		t.Fatalf("baseline replacement error = %v", err)
	}
	entries, err := os.ReadDir(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("replacement symlink target mutated during baseline: %#v", entries)
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
