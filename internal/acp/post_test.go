package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzcli"
)

// Regression: #961, a Buzz CLI whose child kept
// stderr open held cmd.Run, and with it the event's post lock, past the
// post budget, so finals, cancels and the sweep blocked. WaitDelay ends the
// wait inside the budget. The children here never exit on their own, so the
// final can reserve only if the post stops waiting for their pipes; the
// test asserts that it does, not how fast (CI run 39748311658 failed a
// wall-clock bound under load).
func TestStuckBuzzCLIReleasesThePostLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := canonicalTempDir(t)
	started, pids := filepath.Join(dir, "started"), filepath.Join(dir, "pids")
	cli := filepath.Join(dir, "buzz")
	// The CLI exits at once; both sleeps inherit stderr and keep it open
	// until cleanup kills them.
	script := "#!/bin/sh\nsleep 3600 &\necho $! >> '" + pids + "'\nsleep 3600 &\necho $! >> '" + pids + "'\n: > '" + started + "'\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	killSleeps := func() {
		raw, _ := os.ReadFile(pids)
		for _, field := range strings.Fields(string(raw)) {
			if pid, err := strconv.Atoi(field); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	}
	t.Setenv(buzzcli.EnvCLI, cli)
	saved := buzzPort
	buzzPort = buzzcli.New([]string{"BUZZ_PRIVATE_KEY=test"})

	s, _ := mailboxServer(t)
	// A budget long enough that the CLI always runs before it ends: the
	// pipes are released WaitDelay (a tenth of it) after the CLI exits.
	s.cfg.PostTimeout = 10 * time.Second
	eventID := strings.Repeat("8", 63) + "a"
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.postOnce(eventID, postStatus, []byte("x\n"), "6eff60e4-32ab-48ec-bd3d-f4c97872f370", "status")
	}()
	// The post reads buzzPort, so it is restored only after the post
	// returns, which a post stuck on the sleeps' pipes does once they die.
	t.Cleanup(func() {
		killSleeps()
		<-done
		buzzPort = saved
	})

	// The final reserves only once the CLI runs, so the post holds the lock.
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatal("the post ended before the fake CLI ran")
		case <-time.After(10 * time.Millisecond):
		}
	}
	reserved := make(chan error, 1)
	go func() {
		_, err := s.reserveFinal(eventID, []byte(`{"reply_id":"r"}`))
		reserved <- err
	}()
	// Without WaitDelay the post never returns; this bound only ends that
	// hang.
	select {
	case err := <-reserved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the final never got the status post's lock")
	}
}
