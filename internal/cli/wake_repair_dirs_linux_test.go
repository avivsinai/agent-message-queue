//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type linuxRetainedWakeWatcherFixture struct {
	watcher   wakeEventWatcher
	root      string
	agentPath string
	inboxPath string
}

func TestLinuxRetainedWakeWatcherNormalizesCanonicalCreate(t *testing.T) {
	fixture := newLinuxRetainedWakeWatcherForTest(t)
	writeLinuxRetainedWakeWatcherMessage(
		t,
		filepath.Join(fixture.inboxPath, "delivered.md"),
		"delivered",
	)
	assertLinuxRetainedWakeWatcherEvent(t, fixture.watcher, fixture.inboxPath)
}

func newLinuxRetainedWakeWatcherForTest(t *testing.T) linuxRetainedWakeWatcherFixture {
	t.Helper()
	return newLinuxRetainedWakeWatcherForTestWithSetup(t, nil)
}

func newLinuxRetainedWakeWatcherForTestWithSetup(
	t *testing.T,
	setup func(agentPath, inboxPath string),
) linuxRetainedWakeWatcherFixture {
	t.Helper()
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentPath := fsq.AgentBase(root, "codex")
	inboxPath := fsq.AgentInboxNew(root, "codex")
	if setup != nil {
		setup(agentPath, inboxPath)
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
	t.Cleanup(func() {
		closeLinuxRetainedWakeWatcherBounded(t, watcher)
	})
	return linuxRetainedWakeWatcherFixture{
		watcher:   watcher,
		root:      root,
		agentPath: agentPath,
		inboxPath: inboxPath,
	}
}

func closeLinuxRetainedWakeWatcherBounded(t *testing.T, watcher wakeEventWatcher) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		result <- watcher.Close()
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("close retained wake watcher: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("close retained wake watcher did not finish")
	}
}

func writeLinuxRetainedWakeWatcherMessage(t *testing.T, path, subject string) {
	t.Helper()
	message := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       filepath.Base(path),
			From:     "claude",
			To:       []string{"codex"},
			Thread:   "p2p/claude__codex",
			Subject:  subject,
			Created:  "2026-07-24T00:00:00Z",
			Priority: "normal",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatalf("marshal retained watcher message: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write retained watcher message: %v", err)
	}
}

func assertLinuxRetainedWakeWatcherEvent(
	t *testing.T,
	watcher wakeEventWatcher,
	inboxPath string,
) {
	t.Helper()
	select {
	case event, ok := <-watcher.Events():
		if !ok {
			t.Fatal("retained watcher closed before forwarding message trigger")
		}
		want := fsnotify.Event{
			Name: filepath.Join(inboxPath, "retained-inbox-event.md"),
			Op:   fsnotify.Write,
		}
		if event != want {
			t.Fatalf("normalized retained event = %#v, want %#v", event, want)
		}
	case err, ok := <-watcher.Errors():
		t.Fatalf("retained watcher failed on canonical message create: %v ok=%v", err, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("retained watcher did not forward a canonical message scan trigger")
	}
}
