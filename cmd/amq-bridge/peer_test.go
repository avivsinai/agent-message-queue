package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3"
)

func TestPeerAddWritesNothingUntilFingerprintMatches(t *testing.T) {
	srcRoot := newBridgeRoot(t, "codex")
	ensureHostID(t, srcRoot, "box")
	srcKey := testHostKey("box", "1")
	if err := bridge.WriteIdentity(srcRoot, srcKey); err != nil {
		t.Fatal(err)
	}
	static, err := xferv3.StaticFromSeed(srcKey.Private.Seed())
	if err != nil {
		t.Fatal(err)
	}
	record := "host=box generation=1 public=" + hex.EncodeToString(srcKey.Public())
	host, err := xferv3.ParseHost("box")
	if err != nil {
		t.Fatal(err)
	}
	line, err := xferv3.FormatLine(host, record, "ticket-1", static.Public())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := xferv3.ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	fp := parsed.Fingerprint()

	dstRoot := newBridgeRoot(t, "codex")
	ensureHostID(t, dstRoot, "mac")
	if err := runPeerAdd([]string{"--root", dstRoot, "--from-line", line, "--allow-in", "codex"}); err == nil {
		t.Fatal("peer add without confirm wrote a pin")
	}
	assertNoPeerPin(t, dstRoot, "box")

	if err := runPeerAdd([]string{"--root", dstRoot, "--from-line", line, "--allow-in", "codex", "--confirm-fingerprint", "aaaaaaaaaaaaaaaaaaaaaaaaaa"}); err == nil {
		t.Fatal("peer add with a wrong fingerprint wrote a pin")
	}
	assertNoPeerPin(t, dstRoot, "box")

	if err := runPeerAdd([]string{"--root", dstRoot, "--from-line", line, "--allow-in", "codex", "--confirm-fingerprint", fp}); err != nil {
		t.Fatalf("peer add with matching fingerprint: %v", err)
	}
	if _, err := xferv3.LoadPeer(dstRoot, host); err != nil {
		t.Fatalf("LoadPeer after confirm: %v", err)
	}
	if _, _, err := bridge.LoadTrusted(dstRoot, "box"); err != nil {
		t.Fatalf("LoadTrusted after confirm: %v", err)
	}
}

func TestPeerLinePrintsFingerprintOfLocalIdentity(t *testing.T) {
	root := newBridgeRoot(t, "codex")
	ensureHostID(t, root, "mac")
	key := testHostKey("mac", "1")
	if err := bridge.WriteIdentity(root, key); err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	if err := runPeerLine([]string{"--root", root, "--ticket", "pipe-1", "--fingerprint"}); err != nil {
		os.Stdout = old
		t.Fatal(err)
	}
	_ = w.Close()
	os.Stdout = old
	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(buf[:n]))
	if len(got) != xferv3.FingerprintLen {
		t.Fatalf("fingerprint = %q len %d, want %d", got, len(got), xferv3.FingerprintLen)
	}
}

func assertNoPeerPin(t *testing.T, root, hostName string) {
	t.Helper()
	host, err := xferv3.ParseHost(hostName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xferv3.LoadPeer(root, host); err == nil {
		t.Fatal("peer file written before confirm")
	}
	if _, err := os.Lstat(filepath.Join(root, "bridge", "peers", hostName+".json")); !os.IsNotExist(err) {
		t.Fatalf("peer file exists: %v", err)
	}
	if _, err := os.Lstat(bridge.TrustedPath(root, hostName)); !os.IsNotExist(err) {
		t.Fatalf("trusted file written before confirm: %v", err)
	}
}
