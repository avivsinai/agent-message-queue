package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Bead agent-message-queue-fa4 (Pro review of #956, #962): the owner key goes
// only to AMQ_ACP_BUZZ_CLI or a verified Buzz.app CLI, never to a PATH buzz, a
// symlinked path, or a path made from an empty HOME.
func TestBuzzCLIOnlyVerifiedBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Buzz.app is a macOS bundle")
	}
	writeBundle := func(t *testing.T, apps string) string {
		bin := filepath.Join(apps, "Buzz.app", "Contents", "MacOS", "buzz")
		if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return bin
	}
	// Temp dirs on macOS sit under a symlinked /var; resolve so the rows
	// exercise only the symlinks they create.
	realDir := func(t *testing.T) string {
		d, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	refused := func(t *testing.T, sys, home string) {
		t.Helper()
		if got, err := buzzCLIFrom(sys, home); err == nil {
			t.Fatalf("buzzCLIFrom = %q; want an error", got)
		}
	}

	t.Run("override is returned as given", func(t *testing.T) {
		t.Setenv(envBuzzCLI, "/opt/custom/buzz")
		if got, err := buzzCLI(); err != nil || got != "/opt/custom/buzz" {
			t.Fatalf("buzzCLI() = %q, %v; want the override", got, err)
		}
	})
	t.Run("PATH buzz is never selected", func(t *testing.T) {
		binDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(binDir, "buzz"), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		got, err := buzzCLIFrom(realDir(t), realDir(t))
		if err == nil || !strings.Contains(err.Error(), envBuzzCLI) {
			t.Fatalf("buzzCLIFrom = %q, %v; want an error naming %s", got, err, envBuzzCLI)
		}
	})
	t.Run("system bundle is preferred over home", func(t *testing.T) {
		sys, home := realDir(t), realDir(t)
		want := writeBundle(t, sys)
		writeBundle(t, filepath.Join(home, "Applications"))
		if got, err := buzzCLIFrom(sys, home); err != nil || got != want {
			t.Fatalf("buzzCLIFrom = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("home bundle is used when no system bundle", func(t *testing.T) {
		sys, home := realDir(t), realDir(t)
		want := writeBundle(t, filepath.Join(home, "Applications"))
		if got, err := buzzCLIFrom(sys, home); err != nil || got != want {
			t.Fatalf("buzzCLIFrom = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("symlinked Buzz.app is not selected", func(t *testing.T) {
		sys, home, real := realDir(t), realDir(t), realDir(t)
		writeBundle(t, real)
		apps := filepath.Join(home, "Applications")
		if err := os.MkdirAll(apps, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(real, "Buzz.app"), filepath.Join(apps, "Buzz.app")); err != nil {
			t.Fatal(err)
		}
		refused(t, sys, home)
	})
	t.Run("empty or relative home makes no relative candidate", func(t *testing.T) {
		sys, cwd := realDir(t), realDir(t)
		writeBundle(t, filepath.Join(cwd, "Applications"))
		t.Chdir(cwd)
		refused(t, sys, "")
		refused(t, sys, ".")
	})
	t.Run("non-executable CLI is skipped", func(t *testing.T) {
		sys, home := realDir(t), realDir(t)
		bin := writeBundle(t, filepath.Join(home, "Applications"))
		if err := os.Chmod(bin, 0o600); err != nil {
			t.Fatal(err)
		}
		refused(t, sys, home)
	})
	// Symlinks above the bundle root redirect the key (Pro review of #962).
	t.Run("symlinked home Applications is not selected", func(t *testing.T) {
		sys, home, real := realDir(t), realDir(t), realDir(t)
		writeBundle(t, real)
		if err := os.Symlink(real, filepath.Join(home, "Applications")); err != nil {
			t.Fatal(err)
		}
		refused(t, sys, home)
	})
	t.Run("symlinked home is not selected", func(t *testing.T) {
		sys, real := realDir(t), realDir(t)
		writeBundle(t, filepath.Join(real, "Applications"))
		link := filepath.Join(realDir(t), "home")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		refused(t, sys, link)
	})
	t.Run("symlinked system Applications is not selected", func(t *testing.T) {
		home, real := realDir(t), realDir(t)
		writeBundle(t, real)
		link := filepath.Join(realDir(t), "Applications")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		refused(t, link, home)
	})
}

// Review of #961 r8 (agent-message-queue-bdq): a Buzz CLI whose child kept
// stderr open held cmd.Run, and with it the event's post lock, past the
// post budget, so finals, cancels and the sweep blocked. WaitDelay ends the
// wait inside the budget.
func TestStuckBuzzCLIReleasesThePostLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := canonicalTempDir(t)
	started, pids := filepath.Join(dir, "started"), filepath.Join(dir, "pids")
	cli := filepath.Join(dir, "buzz")
	// Both sleeps inherit stderr and outlive the killed shell. Cleanup kills
	// the ones it knows; a sleep the shell could not record ends by itself.
	script := "#!/bin/sh\nsleep 5 &\necho $! >> '" + pids + "'\nsleep 5 &\necho $! >> '" + pids + "'\n: > '" + started + "'\nwait\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		raw, _ := os.ReadFile(pids)
		for _, field := range strings.Fields(string(raw)) {
			if pid, err := strconv.Atoi(field); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	})
	t.Setenv(envBuzzCLI, cli)
	saved := buzzIdentity
	buzzIdentity = []string{"BUZZ_PRIVATE_KEY=test"}

	s, _ := mailboxServer(t)
	s.cfg.PostTimeout = time.Second
	eventID := strings.Repeat("8", 63) + "a"
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.postOnce(eventID, postStatus, []byte("x\n"), "6eff60e4-32ab-48ec-bd3d-f4c97872f370", "status")
	}()
	// The post reads buzzIdentity, so it is restored only after the post
	// returns.
	t.Cleanup(func() {
		<-done
		buzzIdentity = saved
	})

	// Measure from the moment the fake CLI runs, so start latency under load
	// is not part of the bound. If the post ends before the CLI ever runs,
	// the lock is already free and the bound holds from then.
	deadline, hasDeadline := t.Deadline()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-done:
		case <-time.After(10 * time.Millisecond):
			if hasDeadline && time.Now().After(deadline.Add(-5*time.Second)) {
				t.Fatal("the fake CLI never started")
			}
			continue
		}
		break
	}
	begin := time.Now()
	if _, err := s.reserveFinal(eventID, []byte(`{"reply_id":"r"}`)); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > s.cfg.PostTimeout+time.Second {
		t.Fatalf("the final waited %v for the status post's lock", took)
	}
}
