package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Bead agent-message-queue-fa4 (Pro review of #956): the owner key goes only
// to AMQ_ACP_BUZZ_CLI or a verified Buzz.app CLI, never to a PATH buzz, a
// symlinked bundle, or a path made from an empty HOME.
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
	setup := func(t *testing.T) (home string) {
		home = realDir(t)
		t.Setenv("HOME", home)
		t.Setenv(envBuzzCLI, "")
		systemApplications = realDir(t)
		t.Cleanup(func() { systemApplications = "/Applications" })
		return home
	}

	t.Run("valid bundle wins over PATH", func(t *testing.T) {
		home := setup(t)
		want := writeBundle(t, filepath.Join(home, "Applications"))
		binDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(binDir, "buzz"), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		if got, err := buzzCLI(); err != nil || got != want {
			t.Fatalf("buzzCLI() = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("PATH buzz without a bundle is refused", func(t *testing.T) {
		setup(t)
		binDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(binDir, "buzz"), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)
		if got, err := buzzCLI(); err == nil {
			t.Fatalf("buzzCLI() = %q; want an error naming %s", got, envBuzzCLI)
		}
	})
	t.Run("symlinked Buzz.app is not selected", func(t *testing.T) {
		home := setup(t)
		real := realDir(t)
		writeBundle(t, real)
		apps := filepath.Join(home, "Applications")
		if err := os.MkdirAll(apps, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(real, "Buzz.app"), filepath.Join(apps, "Buzz.app")); err != nil {
			t.Fatal(err)
		}
		if got, err := buzzCLI(); err == nil {
			t.Fatalf("buzzCLI() = %q through a symlinked Buzz.app", got)
		}
	})
	t.Run("empty HOME makes no relative candidate", func(t *testing.T) {
		setup(t)
		cwd := t.TempDir()
		writeBundle(t, filepath.Join(cwd, "Applications"))
		t.Chdir(cwd)
		t.Setenv("HOME", "")
		if got, err := buzzCLI(); err == nil {
			t.Fatalf("buzzCLI() = %q from a relative path", got)
		}
	})
	t.Run("non-executable CLI is skipped", func(t *testing.T) {
		home := setup(t)
		bin := writeBundle(t, filepath.Join(home, "Applications"))
		if err := os.Chmod(bin, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := buzzCLI(); err == nil {
			t.Fatalf("buzzCLI() = %q for a file without an execute bit", got)
		}
	})
	// Pro review of #962: symlinks above the bundle root redirect the key.
	t.Run("symlinked home Applications is not selected", func(t *testing.T) {
		home := setup(t)
		real := realDir(t)
		writeBundle(t, real)
		if err := os.Symlink(real, filepath.Join(home, "Applications")); err != nil {
			t.Fatal(err)
		}
		if got, err := buzzCLI(); err == nil {
			t.Fatalf("buzzCLI() = %q through a symlinked Applications", got)
		}
	})
	t.Run("symlinked home is not selected", func(t *testing.T) {
		setup(t)
		real := realDir(t)
		writeBundle(t, filepath.Join(real, "Applications"))
		link := filepath.Join(realDir(t), "home")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", link)
		if got, err := buzzCLI(); err == nil {
			t.Fatalf("buzzCLI() = %q through a symlinked home", got)
		}
	})
	t.Run("unusable system candidate falls through to home", func(t *testing.T) {
		home := setup(t)
		sys := writeBundle(t, systemApplications)
		if err := os.Chmod(sys, 0o600); err != nil {
			t.Fatal(err)
		}
		want := writeBundle(t, filepath.Join(home, "Applications"))
		if got, err := buzzCLI(); err != nil || got != want {
			t.Fatalf("buzzCLI() = %q, %v; want %q", got, err, want)
		}
	})
}
