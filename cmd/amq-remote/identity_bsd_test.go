//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// bbn r4: the macOS identity comes from the FIRST txt record, which is the
// program text; matching is by full path, and a first record that is not
// amq-acp skips the process without searching other records (dyld never
// decides).
func TestLsofIdentityParsesTxtRecord(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(pid int) ([]byte, error) {
		return []byte("  111\n" +
			"ftxt\ni175708548\nn/opt/homebrew/Cellar/amq/0.83.0/bin/amq-acp\n" +
			"ftxt\ni1152921500312573256\nn/usr/lib/dyld\n"), nil
	}
	id, err := processIdentityOS(111)
	if err != nil {
		t.Fatal(err)
	}
	if id.Inode != 175708548 {
		t.Fatalf("identity = %+v, want the amq-acp record, not dyld", id)
	}
}

// bbn r4: a first txt record that is not amq-acp means the process is not
// an amq-acp candidate; later records are never searched.
func TestLsofIdentitySkipsWhenFirstRecordIsNotAMQ(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(pid int) ([]byte, error) {
		// dyld first, amq-acp second: basename search would wrongly take the
		// second record.
		return []byte("  111\n" +
			"ftxt\ni1152921500312573256\nn/usr/lib/dyld\n" +
			"ftxt\ni175708548\nn/opt/homebrew/Cellar/amq/0.83.0/bin/amq-acp\n"), nil
	}
	if _, err := processIdentityOS(111); err == nil {
		t.Fatal("want an error when the first txt record is not amq-acp")
	}
}

// A process with no readable first txt record has no identity and is
// skipped, never guessed.
func TestLsofIdentityWithoutAMQRecordIsAnError(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(pid int) ([]byte, error) {
		return []byte("  111\nftxt\ni2\nn/usr/lib/dyld\n"), nil
	}
	if _, err := processIdentityOS(111); err == nil {
		t.Fatal("want an error when no txt record names amq-acp")
	}
}

// bbn r4: lsof is bounded; a timed-out probe yields no identity, and the
// caller skips the process.
func TestLsofTimeoutSkipsTheProcess(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(pid int) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	if _, err := processIdentityOS(111); err == nil {
		t.Fatal("want an error on a timed-out probe")
	}
}

// The installed identity stats the real file behind the sibling symlink.
func TestInstalledIdentityResolvesTheSibling(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "amq-acp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := installedACPPath
	t.Cleanup(func() { installedACPPath = origPath })
	installedACPPath = func() (string, error) { return bin, nil }

	id, err := installedIdentityOS()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if id.Inode != inodeOf(info) {
		t.Fatalf("identity = %+v, want the file's own inode", id)
	}
}

func inodeOf(info os.FileInfo) uint64 { id, _ := statIdentity(info); return id.Inode }
