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

// Regression: #962, the owner key goes
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
	// Symlinks above the bundle root redirect the key.
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
	t.Setenv(envBuzzCLI, cli)
	saved := buzzIdentity
	buzzIdentity = []string{"BUZZ_PRIVATE_KEY=test"}

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
	// The post reads buzzIdentity, so it is restored only after the post
	// returns, which a post stuck on the sleeps' pipes does once they die.
	t.Cleanup(func() {
		killSleeps()
		<-done
		buzzIdentity = saved
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
