package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuzzCLIPrefersAppBundleOverPATH: with a buzz on PATH and a Buzz.app
// bundle both present, the bundle must win — the owner key is passed only to
// the Buzz.app CLI (agent-message-queue-37m, Ben review F11).
func TestBuzzCLIPrefersAppBundleOverPATH(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	bundleBin := filepath.Join(tmp, "Applications", "Buzz.app", "Contents", "MacOS", "buzz")
	if err := os.MkdirAll(filepath.Dir(bundleBin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundleBin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pathBin := filepath.Join(binDir, "buzz")
	if err := os.WriteFile(pathBin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if p, err := exec.LookPath("buzz"); err != nil || p != pathBin {
		t.Fatalf("setup: fake PATH buzz not first on PATH (got %q, %v)", p, err)
	}
	got, err := buzzCLI()
	if err != nil {
		t.Fatal(err)
	}
	if got == pathBin {
		t.Fatalf("buzzCLI() = %q; a PATH buzz must not win over the Buzz.app bundle", got)
	}
	if !strings.Contains(got, "Buzz.app") || !strings.HasSuffix(got, filepath.Join("MacOS", "buzz")) {
		t.Fatalf("buzzCLI() = %q; want a Buzz.app bundle path", got)
	}
}
