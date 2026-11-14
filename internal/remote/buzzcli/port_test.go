package buzzcli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
		t.Setenv(EnvCLI, "/opt/custom/buzz")
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
		if err == nil || !strings.Contains(err.Error(), EnvCLI) {
			t.Fatalf("buzzCLIFrom = %q, %v; want an error naming %s", got, err, EnvCLI)
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
	// Field defect 2026-11-14: Buzz Desktop 1.0.2 ships
	// /Applications/Buzz.app/Contents/MacOS/Buzz (only the GUI binary, no
	// "buzz" CLI). On a case-insensitive volume the path lookup for "buzz"
	// resolves to "Buzz", EvalSymlinks returns the path as given, and the
	// key went to the wrong program. The new entry-name check rejects it.
	// The pre-fix failure of this row only reproduces on a case-insensitive
	// volume (macOS, this Mac); on an ext4 Linux runner the path lookup
	// already failed and bundledBuzz returned "" for the wrong reason.
	t.Run("an entry Buzz without buzz is not the CLI", func(t *testing.T) {
		sys, home := realDir(t), realDir(t)
		wrongBin := filepath.Join(sys, "Buzz.app", "Contents", "MacOS", "Buzz")
		if err := os.MkdirAll(filepath.Dir(wrongBin), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wrongBin, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if got := bundledBuzz(filepath.Join(sys, "Buzz.app")); got != "" {
			t.Fatalf("bundledBuzz = %q; want \"\" (entry Buzz, no buzz)", got)
		}
		want := writeBundle(t, filepath.Join(home, "Applications"))
		if got, err := buzzCLIFrom(sys, home); err != nil || got != want {
			t.Fatalf("buzzCLIFrom = %q, %v; want the home CLI %q", got, err, want)
		}
	})
}
