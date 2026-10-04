//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sampleLsof emits -FpfDin output: a pid line, then per txt record an f,
// D (hex device), i (decimal inode), and n (name) field record.
func sampleLsof(pid int, recs ...[3]string) []byte {
	out := []string{"p" + strconv.Itoa(pid)}
	for _, r := range recs {
		out = append(out, "ftxt", "D"+r[0], "i"+r[1], "n"+r[2])
	}
	return []byte(strings.Join(out, "\n"))
}

// bbn r4/r5: the macOS identity comes from the FIRST txt record (the program
// text), with device and inode; a later record (dyld) is never searched.
func TestLsofIdentityParsesTxtRecord(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(ctx context.Context, pid int) ([]byte, error) {
		return sampleLsof(111,
			[3]string{"0x1000010", "175708548", "/opt/homebrew/Cellar/amq/0.83.0/bin/amq-acp"},
			[3]string{"0x1000010", "1152921500312573256", "/usr/lib/dyld"},
		), nil
	}
	id, err := processIdentityOS(context.Background(), 111)
	if err != nil {
		t.Fatal(err)
	}
	if id.Dev != 0x1000010 || id.Inode != 175708548 {
		t.Fatalf("identity = %+v, want the amq-acp record, not dyld", id)
	}
}

// bbn r4: a first txt record that is not amq-acp means the process is not
// an amq-acp candidate; the amq-acp second record is never searched.
func TestLsofIdentitySkipsWhenFirstRecordIsNotAMQ(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(ctx context.Context, pid int) ([]byte, error) {
		return sampleLsof(111,
			[3]string{"0x1000010", "1152921500312573256", "/usr/lib/dyld"},
			[3]string{"0x1000010", "175708548", "/opt/homebrew/Cellar/amq/0.83.0/bin/amq-acp"},
		), nil
	}
	if _, err := processIdentityOS(context.Background(), 111); err == nil {
		t.Fatal("want an error when the first txt record is not amq-acp")
	}
}

// bbn r5: strict — a non-absolute name is unknown, never guessed.
func TestLsofIdentityRejectsNonAbsoluteName(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(ctx context.Context, pid int) ([]byte, error) {
		return sampleLsof(111, [3]string{"0x1000010", "175708548", "amq-acp"}), nil
	}
	if _, err := processIdentityOS(context.Background(), 111); err == nil {
		t.Fatal("want an error for a non-absolute first txt name")
	}
}

// bbn r5: strict — a missing or zero device is unknown.
func TestLsofIdentityRejectsMissingDevice(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(ctx context.Context, pid int) ([]byte, error) {
		// no D field at all
		return []byte("p111\nftxt\ni175708548\nn/opt/homebrew/Cellar/amq/0.83.0/bin/amq-acp\n"), nil
	}
	if _, err := processIdentityOS(context.Background(), 111); err == nil {
		t.Fatal("want an error when the first txt record has no device")
	}
}

// bbn r5: strict — a zero inode is unknown.
func TestLsofIdentityRejectsZeroInode(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(ctx context.Context, pid int) ([]byte, error) {
		return sampleLsof(111, [3]string{"0x1000010", "0", "/opt/homebrew/Cellar/amq/0.83.0/bin/amq-acp"}), nil
	}
	if _, err := processIdentityOS(context.Background(), 111); err == nil {
		t.Fatal("want an error when the first txt record has a zero inode")
	}
}

// A process with no readable first txt record has no identity and is
// skipped, never guessed.
func TestLsofIdentityWithoutAMQRecordIsAnError(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(ctx context.Context, pid int) ([]byte, error) {
		return []byte("p111\nftxt\nD0x1\ni2\nn/usr/lib/dyld\n"), nil
	}
	if _, err := processIdentityOS(context.Background(), 111); err == nil {
		t.Fatal("want an error when no txt record names amq-acp")
	}
}

// bbn r5: a timed-out probe (context exceeded) yields no identity, and the
// caller treats the process as unknown.
func TestLsofTimeoutSkipsTheProcess(t *testing.T) {
	orig := lsofCLI
	t.Cleanup(func() { lsofCLI = orig })
	lsofCLI = func(ctx context.Context, pid int) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	if _, err := processIdentityOS(context.Background(), 111); err == nil {
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
