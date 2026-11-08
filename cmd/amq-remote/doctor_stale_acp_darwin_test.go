//go:build darwin

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Review of the version store: a long-running amq-acp runs from its copy in
// ~/.amq/state/versions, whose file identity never matches the installed
// amq-acp, so doctor told the owner to restart every current Buzz agent.
func TestDoctorTreatsTheStoreCopyOfTheInstalledACPAsCurrent(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	content := []byte("amq-acp image")
	installed := filepath.Join(t.TempDir(), "amq-acp")
	if err := os.WriteFile(installed, content, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	entry := filepath.Join(home, ".amq", "state", "versions", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(entry, "amq-acp")
	if err := os.WriteFile(stored, content, 0o755); err != nil {
		t.Fatal(err)
	}

	origList, origID, origInst, origPath := listACPProcesses, processIdentity, installedIdentity, installedACPPath
	t.Cleanup(func() {
		listACPProcesses, processIdentity, installedIdentity, installedACPPath = origList, origID, origInst, origPath
	})
	listACPProcesses = func(context.Context) ([]acpProcess, error) {
		return []acpProcess{{PID: 111, Name: "amq-acp"}}, nil
	}
	processIdentity = func(context.Context, int) (execIdentity, error) {
		return execIdentity{Dev: testInstalled.Dev, Inode: testInstalled.Inode + 1, Path: stored}, nil
	}
	installedIdentity = func() (execIdentity, error) { return testInstalled, nil }
	installedACPPath = func() (string, error) { return installed, nil }

	probes := inspectStaleACP(t)
	if len(probes) != 1 || probes[0].Stale || probes[0].Reason != "" {
		t.Fatalf("probes = %+v, want the store copy of the installed amq-acp current", probes)
	}
}
