package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// pacedPasses runs pass in a worker paced by wake until the test ends; each pass's pace arrives on the result.
func pacedPasses(t *testing.T, s *Server, wake *wakeup, pass func(time.Time) pace) <-chan pace {
	t.Helper()
	passes := make(chan pace, 1024)
	quit, done := make(chan struct{}), make(chan struct{})
	go s.paced(quit, done, wake, func(now time.Time) pace {
		p := pass(now)
		passes <- p
		return p
	})
	t.Cleanup(func() { close(quit); <-done })
	return passes
}

// settled waits until a pass asks for nothing within 30 s and no pass
// follows it, so only a change can wake the worker before the test ends.
func settled(t *testing.T, passes <-chan pace) {
	t.Helper()
	deadline := time.After(4 * time.Second)
	quiet := false
	for {
		select {
		case p := <-passes:
			quiet = p.next.IsZero() || time.Until(p.next) > 30*time.Second
		case <-time.After(300 * time.Millisecond):
			if quiet {
				return
			}
		case <-deadline:
			t.Fatal("the worker never settled")
		}
	}
}

// within fails unless cond holds within 3 s.
func within(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s did not happen without a tick", what)
}

// A settled machine agent's dialog worker posts a dialog its seam opens at
// once, with no tick to wait for (bead agent-message-queue-udg).
func TestDialogWorkerWakesOnANewDialog(t *testing.T) {
	s, _ := machineServer(t)
	b := liveSession(t, "alpha")
	activeChannel(t, s, b, machineChanA)
	dir, err := dialog.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var posts []string
	savedPost, savedRead := postAnswer, readReactions
	t.Cleanup(func() { postAnswer, readReactions = savedPost, savedRead })
	postAnswer = func(channel, content string, _ time.Duration) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, channel)
		return strings.Repeat("a", 64), nil
	}
	readReactions = func(string, time.Duration) ([]reaction, error) { return nil, nil }
	passes := pacedPasses(t, s, newWakeup("dialog", 50*time.Millisecond, time.Hour), s.dialogTick)
	settled(t, passes)

	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", Text: "$ ls", OpenedAt: time.Now(), PID: os.Getpid(),
		Options: []dialog.Option{{N: 1, Label: "Allow once", Value: "allow"}}}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	within(t, "the dialog post", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(posts) == 1 && posts[0] == machineChanA
	})
}

// A settled mirror worker posts a turn that ends in a watched transcript
// at once, with no tick to wait for (bead agent-message-queue-udg).
func TestMirrorWorkerWakesOnAFinishedTurn(t *testing.T) {
	s, _ := machineServer(t)
	root := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	f := &mirrorFixture{t: t, transcript: filepath.Join(canonicalTempDir(t), "session.jsonl")}
	if err := os.WriteFile(f.transcript, []byte(`{"type":"user","uuid":"o0","promptId":"p0","timestamp":"2026-11-04T09:00:00Z","message":{"content":"earlier"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Holder: holder, Name: "alpha",
		Label: "claude · proj/alpha @ box", Harness: "claude", Transcript: f.transcript}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, s, b, machineChanA)
	var mu sync.Mutex
	var posts []string
	savedPost := postAnswer
	t.Cleanup(func() { postAnswer = savedPost })
	postAnswer = func(_, content string, _ time.Duration) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, content)
		return strings.Repeat("a", 64), nil
	}
	passes := pacedPasses(t, s, newWakeup("mirror", 50*time.Millisecond, time.Hour), s.mirrorTick)
	settled(t, passes)

	f.add(f.userLine("o1", "p1", 0, "ship", nil), f.assistantLine("m1", 1, "end_turn", textBlock("Shipped.")))
	within(t, "the turn post", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(posts) == 1 && posts[0] == "▶ ship\n✓ 1s\nShipped."
	})
}

// A settled machine worker provisions a session bound after it settled at
// once, with no tick to wait for (bead agent-message-queue-udg).
func TestMachineWorkerWakesOnANewBinding(t *testing.T) {
	s, rec := machineServer(t)
	passes := pacedPasses(t, s, newWakeup("machine", 50*time.Millisecond, time.Hour), func(now time.Time) pace {
		return s.machineTick(context.Background(), now)
	})
	settled(t, passes)

	liveSession(t, "alpha")
	within(t, "the channel create", func() bool { return rec.count("channels create") == 1 })
}

// A worker whose watcher cannot start passes every base interval, as with
// a tick (bead agent-message-queue-udg).
func TestWorkerWithoutAWatcherTicks(t *testing.T) {
	saved := newWatcher
	t.Cleanup(func() { newWatcher = saved })
	newWatcher = func() (*fsnotify.Watcher, error) { return nil, errors.New("too many open files") }
	wake := newWakeup("dialog", 20*time.Millisecond, time.Hour)
	if wake.watching() {
		t.Fatal("a worker with no watcher reports watching")
	}
	passes := pacedPasses(t, &Server{}, wake, func(time.Time) pace { return pace{} })
	for range 5 {
		select {
		case <-passes:
		case <-time.After(2 * time.Second):
			t.Fatal("the worker without a watcher stopped passing")
		}
	}
}
