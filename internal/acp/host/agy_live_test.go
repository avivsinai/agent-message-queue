package host

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// execAgy is a real agy process for the opt-in live proof.
type execAgy struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	done chan struct{}
}

func (a *execAgy) Stdin() io.WriteCloser { return a.in }
func (a *execAgy) Stdout() io.Reader     { return a.out }
func (a *execAgy) Interrupt() error      { return a.cmd.Process.Signal(os.Interrupt) }
func (a *execAgy) Stop() {
	_ = a.in.Close()
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		_ = a.cmd.Process.Kill()
		<-a.done
	}
}

func spawnRealAgy(t *testing.T, dir string) Spawner {
	return func(extra []string) (AgentProcess, error) {
		cmd := exec.Command("agy", extra...)
		cmd.Dir = dir
		cmd.Stderr = os.Stderr
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		a := &execAgy{cmd: cmd, in: in, out: out, done: make(chan struct{})}
		go func() { _ = cmd.Wait(); close(a.done) }()
		t.Logf("started agy %s", strings.Join(extra, " "))
		return a, nil
	}
}

// waitReplies waits until codex's inbox holds n messages and returns the
// newest one's body.
func waitReplies(t *testing.T, root string, n int, timeout time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, "agents", "codex", "inbox", "new")
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		entries, _ := os.ReadDir(dir)
		if len(entries) >= n {
			msg, err := format.ReadMessageFile(filepath.Join(dir, entries[len(entries)-1].Name()))
			if err != nil {
				t.Fatal(err)
			}
			return msg.Body
		}
	}
	t.Fatalf("codex has fewer than %d replies after %s", n, timeout)
	return ""
}

// runAgyHostFor serves the inbox through real agy until n replies exist.
func runAgyHostFor(t *testing.T, root, dir string, n int) string {
	t.Helper()
	h, err := Open(Config{Root: root, Me: "grok", Cwd: dir, Version: "live", Agent: "agy", Log: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.RunAgy(ctx, spawnRealAgy(t, dir)) }()
	// Stop agy and the host before the lock is released, also when an
	// assertion fails.
	var runErr error
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			runErr = <-done
			h.Close()
		}
	}
	defer stop()
	body := waitReplies(t, root, n, 3*time.Minute)
	stop()
	if runErr != nil {
		t.Fatalf("agy host: %v", runErr)
	}
	return body
}

// TestAgyLiveRemembersAcrossHostRestart drives the installed agy: two
// messages with a host restart between them; the second answer must
// remember the first through the recorded conversation.
func TestAgyLiveRemembersAcrossHostRestart(t *testing.T) {
	if os.Getenv("AMQ_AGY_LIVE") != "1" {
		t.Skip("set AMQ_AGY_LIVE=1 to run against the installed agy")
	}
	root := newRoot(t)
	dir := t.TempDir()

	send(t, root, "codex", format.KindQuestion, "Remember the number 41 for later. Reply with the single word ok.")
	first := runAgyHostFor(t, root, dir, 1)
	t.Logf("first reply: %q", first)

	send(t, root, "codex", format.KindQuestion, "Which number did I ask you to remember? Reply with the number only.")
	second := runAgyHostFor(t, root, dir, 2)
	t.Logf("second reply: %q", second)
	if !strings.Contains(second, "41") {
		t.Fatalf("second reply %q does not remember 41 across the restart", second)
	}
}
