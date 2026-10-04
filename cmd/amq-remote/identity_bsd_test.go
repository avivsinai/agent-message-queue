//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// bbn r4: the macOS identity comes from the lsof txt record whose name
// basename is amq-acp, carrying device and inode; dyld records never decide.
func TestLsofIdentityParsesTxtRecord(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(pid int) ([]byte, error) {
		return []byte("  111\n" +
			"ftxt\nD0x1000010\ni175708548\nn/opt/homebrew/Cellar/amq/0.83.0/bin/amq-acp\n" +
			"ftxt\nD0x1000010\ni1152921500312573256\nn/usr/lib/dyld\n"), nil
	}
	id, err := processIdentityOS(111)
	if err != nil {
		t.Fatal(err)
	}
	if id.Dev != 0x1000010 || id.Inode != 175708548 {
		t.Fatalf("identity = %+v, want the amq-acp record, not dyld", id)
	}
}

// A process with no amq-acp txt record has no readable identity and is
// skipped, never guessed.
func TestLsofIdentityWithoutAMQRecordIsAnError(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(pid int) ([]byte, error) {
		return []byte("  111\nftxt\nD0x1\ni2\nn/usr/lib/dyld\n"), nil
	}
	if _, err := processIdentityOS(111); err == nil {
		t.Fatal("want an error when no txt record names amq-acp")
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
	if id.Inode != inodeOf(info) || id.Dev != devOf(info) {
		t.Fatalf("identity = %+v, want the file's own dev/inode", id)
	}
}

func inodeOf(info os.FileInfo) uint64 { id, _ := statIdentity(info); return id.Inode }
func devOf(info os.FileInfo) uint64   { id, _ := statIdentity(info); return id.Dev }
