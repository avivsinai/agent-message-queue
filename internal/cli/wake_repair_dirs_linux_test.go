//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLinuxRetainedWakeWatcherFailsOnDirectInboxLossWithoutForwarding(t *testing.T) {
	for _, test := range []struct {
		name string
		lose func(*testing.T, string)
	}{
		{
			name: "rename",
			lose: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Rename(path, filepath.Join(filepath.Dir(path), "renamed-detached")); err != nil {
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
		{
			name: "delete and recreate",
			lose: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("delete retained inbox: %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("recreate retained inbox: %v", err)
				}
			},
		},
		{
			name: "rename and recreate",
			lose: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Rename(path, filepath.Join(filepath.Dir(path), "replaced-detached")); err != nil {
					t.Fatalf("rename retained inbox: %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("recreate retained inbox: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLinuxRetainedWakeWatcherForTest(t)
			test.lose(t, fixture.inboxPath)
			assertLinuxRetainedWakeWatcherTerminalWithoutEvents(t, fixture.watcher)
		})
	}
}

func TestLinuxRetainedWakeWatcherFailsOnAncestorReplacementBeforeDetachedDelivery(t *testing.T) {
	fixture := newLinuxRetainedWakeWatcherForTest(t)
	detachedAgentPath := fixture.agentPath + ".detached"
	if err := os.Rename(fixture.agentPath, detachedAgentPath); err != nil {
		t.Fatalf("rename retained agent directory: %v", err)
	}
	if err := fsq.EnsureAgentDirs(fixture.root, "codex"); err != nil {
		t.Fatalf("recreate canonical agent directory: %v", err)
	}
	writeLinuxRetainedWakeWatcherMessage(
		t,
		filepath.Join(detachedAgentPath, "inbox", "new", "late.md"),
		"detached late delivery",
	)
	assertLinuxRetainedWakeWatcherTerminalWithoutEvents(t, fixture.watcher)
}

func TestLinuxRetainedWakeWatcherIgnoresAgentMetadata(t *testing.T) {
	fixture := newLinuxRetainedWakeWatcherForTest(t)
	if err := os.WriteFile(
		filepath.Join(fixture.agentPath, "presence.json"),
		[]byte(`{"status":"active"}`),
		0o600,
	); err != nil {
		t.Fatalf("write agent metadata: %v", err)
	}

	select {
	case event, ok := <-fixture.watcher.Events():
		t.Fatalf("agent metadata produced message scan event = %#v ok=%v", event, ok)
	case err, ok := <-fixture.watcher.Errors():
		t.Fatalf("agent metadata failed retained watcher: %v ok=%v", err, ok)
	case <-time.After(200 * time.Millisecond):
	}

	writeLinuxRetainedWakeWatcherMessage(
		t,
		filepath.Join(fixture.inboxPath, "after-metadata.md"),
		"after metadata",
	)
	assertLinuxRetainedWakeWatcherEvent(t, fixture.watcher, fixture.inboxPath)
}

func TestLinuxRetainedWakeWatcherCloseIsIdempotentAndBounded(t *testing.T) {
	fixture := newLinuxRetainedWakeWatcherForTest(t)
	results := make(chan [2]error, 1)
	go func() {
		results <- [2]error{fixture.watcher.Close(), fixture.watcher.Close()}
	}()

	select {
	case result := <-results:
		if result[0] != nil || result[1] != nil {
			t.Fatalf("idempotent retained watcher close = (%v, %v)", result[0], result[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idempotent retained watcher close did not finish")
	}
}

func newLinuxRetainedWakeWatcherForTest(t *testing.T) linuxRetainedWakeWatcherFixture {
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
	t.Cleanup(func() {
		closeLinuxRetainedWakeWatcherBounded(t, watcher)
	})
	return linuxRetainedWakeWatcherFixture{
		watcher:   watcher,
		root:      root,
		agentPath: fsq.AgentBase(root, "codex"),
		inboxPath: fsq.AgentInboxNew(root, "codex"),
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

func assertLinuxRetainedWakeWatcherTerminalWithoutEvents(
	t *testing.T,
	watcher wakeEventWatcher,
) {
	t.Helper()
	events := watcher.Events()
	errorsCh := watcher.Errors()
	var terminalErr error
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	for events != nil || errorsCh != nil {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			t.Fatalf("terminal retained watcher forwarded event: %#v", event)
		case err, ok := <-errorsCh:
			if !ok {
				errorsCh = nil
				continue
			}
			if err == nil {
				t.Fatal("terminal retained watcher reported an empty error")
			}
			if terminalErr != nil {
				t.Fatalf("terminal retained watcher reported multiple errors: %v and %v", terminalErr, err)
			}
			terminalErr = err
		case <-timer.C:
			t.Fatal("terminal retained watcher did not close its event and error channels")
		}
	}
	if terminalErr == nil {
		t.Fatal("retained watcher closed without a terminal namespace error")
	}
	if !strings.Contains(terminalErr.Error(), "retained wake") {
		t.Fatalf("terminal retained watcher error = %v", terminalErr)
	}
}
