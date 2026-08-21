package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
)

func TestApplyFileVerifiesAndCommitsEnvelopeWithReceipt(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	ensureHostID(t, root, "mac")
	ensureTrusted(t, root, "grok-host")
	env := applyFileTestEnvelope(t, "apply-file", testMessage(t, "apply-file-message", "apply-file-thread", "codex", "from bot"))
	path := writeApplyFileEnvelope(t, root, "envelope.json", env)

	if err := runApplyFile([]string{"--root", root, "--file", path}); err != nil {
		t.Fatalf("runApplyFile: %v", err)
	}

	committed := filepath.Join(root, "agents", "claude", "inbox", "new", bridge.TransferFilename(env.SourceHost, env.TransferID))
	got, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(env.Payload) {
		t.Fatalf("committed payload = %q, want %q", got, env.Payload)
	}
	receipt := readApplyReceipt(t, root, env)
	if receipt.Stage != ReceiptDestinationMaildirCommit || receipt.Replayed || receipt.CommittedPath != committed {
		t.Fatalf("receipt = %#v, want first destination commit at %s", receipt, committed)
	}
}

func TestApplyFileReplayAndConflictPreserveMaildirAndReceipt(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	ensureHostID(t, root, "mac")
	ensureTrusted(t, root, "grok-host")
	env := applyFileTestEnvelope(t, "replay-file", []byte("first"))
	path := writeApplyFileEnvelope(t, root, "envelope.json", env)

	if err := runApplyFile([]string{"--root", root, "--file", path}); err != nil {
		t.Fatalf("first runApplyFile: %v", err)
	}
	if err := runApplyFile([]string{"--root", root, "--file", path}); err != nil {
		t.Fatalf("replay runApplyFile: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "agents", "claude", "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries after replay = %d, want 1", len(entries))
	}

	conflict := applyFileTestEnvelope(t, env.TransferID, []byte("different"))
	writeApplyFileEnvelope(t, root, "envelope.json", conflict)
	err = runApplyFile([]string{"--root", root, "--file", path})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("conflict error = %v, want EEXIST", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "agents", "claude", "inbox", "new", bridge.TransferFilename(env.SourceHost, env.TransferID)))
	if err != nil || string(got) != "first" {
		t.Fatalf("conflict changed committed payload: %q %v", got, err)
	}
	receipt := readApplyReceipt(t, root, env)
	if receipt.PayloadSHA256 != env.PayloadSHA256 {
		t.Fatalf("conflict changed receipt digest: %#v", receipt)
	}
}

func TestApplyFileRejectsForgedSourceHostBeforeMaildirCommit(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	ensureHostID(t, root, "mac")
	ensureTrusted(t, root, "grok-host")
	env := applyFileTestEnvelope(t, "forged-file", []byte("forged"))
	if err := bridge.SignEnvelope(&env, testHostKey("attacker", "1")); err != nil {
		t.Fatal(err)
	}
	path := writeApplyFileEnvelope(t, root, "forged.json", env)

	err := runApplyFile([]string{"--root", root, "--file", path})
	if err == nil || !strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("forged source error = %v, want authentication refusal", err)
	}
	assertApplyNotCommitted(t, root, env)
}

func TestApplyFileRejectsUnsignedEnvelopeBeforeMaildirCommit(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	ensureHostID(t, root, "mac")
	ensureTrusted(t, root, "grok-host")
	env := applyFileTestEnvelope(t, "unsigned-file", []byte("unsigned"))
	env.Signature = ""
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	path := writeApplyFileRaw(t, root, "unsigned.json", raw)

	if err := runApplyFile([]string{"--root", root, "--file", path}); err == nil {
		t.Fatal("unsigned envelope was accepted")
	}
	assertApplyNotCommitted(t, root, env)
}

func TestApplyFileRejectsForeignDestinationAndSymlink(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	ensureHostID(t, root, "mac")
	ensureTrusted(t, root, "grok-host")
	env := applyFileTestEnvelope(t, "foreign-file", []byte("foreign"))
	env.DestAlias = "other/claude"
	if err := bridge.SignEnvelope(&env, testHostKey("grok-host", "1")); err != nil {
		t.Fatal(err)
	}
	path := writeApplyFileEnvelope(t, root, "foreign.json", env)
	if err := runApplyFile([]string{"--root", root, "--file", path}); err == nil || !strings.Contains(err.Error(), "local host-id") {
		t.Fatalf("foreign destination error = %v, want local host refusal", err)
	}
	assertApplyNotCommitted(t, root, env)

	env = applyFileTestEnvelope(t, "symlink-file", []byte("symlink"))
	realPath := writeApplyFileEnvelope(t, root, "real.json", env)
	symlinkPath := filepath.Join(root, applyDropRelDir, "symlink.json")
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := runApplyFile([]string{"--root", root, "--file", symlinkPath}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v, want symlink refusal", err)
	}
	assertApplyNotCommitted(t, root, env)
}

func TestApplyFileRejectsTrailingJSON(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	ensureHostID(t, root, "mac")
	ensureTrusted(t, root, "grok-host")
	env := applyFileTestEnvelope(t, "trailing-file", []byte("trailing"))
	path := writeApplyFileEnvelope(t, root, "trailing.json", env)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("\n{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runApplyFile([]string{"--root", root, "--file", path})
	if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing JSON error = %v", err)
	}
	assertApplyNotCommitted(t, root, env)
}

func TestApplyFileRejectsFileOutsideDropDirectory(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	ensureHostID(t, root, "mac")
	ensureTrusted(t, root, "grok-host")
	env := applyFileTestEnvelope(t, "outside-file", []byte("outside"))
	raw, err := bridge.MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "outside.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runApplyFile([]string{"--root", root, "--file", path}); err == nil || !strings.Contains(err.Error(), "under") {
		t.Fatalf("outside drop error = %v, want drop-directory refusal", err)
	}
	assertApplyNotCommitted(t, root, env)
}

func applyFileTestEnvelope(t *testing.T, transferID string, payload []byte) bridge.Envelope {
	t.Helper()
	sum := sha256.Sum256(payload)
	env := bridge.Envelope{
		Version:         bridge.EnvelopeVersion,
		TransferID:      transferID,
		SourceHost:      "grok-host",
		SourceHandle:    "codex",
		DestAlias:       "mac/claude",
		SourceMessageID: transferID + "-message",
		ThreadID:        transferID + "-thread",
		PayloadSHA256:   hex.EncodeToString(sum[:]),
		KeyGeneration:   "1",
		Payload:         payload,
	}
	if err := bridge.SignEnvelope(&env, testHostKey("grok-host", "1")); err != nil {
		t.Fatal(err)
	}
	return env
}

func writeApplyFileEnvelope(t *testing.T, root, name string, env bridge.Envelope) string {
	t.Helper()
	raw, err := bridge.MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	return writeApplyFileRaw(t, root, name, raw)
}

func writeApplyFileRaw(t *testing.T, root, name string, raw []byte) string {
	t.Helper()
	drop := filepath.Join(root, applyDropRelDir)
	if err := os.MkdirAll(drop, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(drop, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readApplyReceipt(t *testing.T, root string, env bridge.Envelope) Receipt {
	t.Helper()
	path := filepath.Join(root, applyReceiptRelDir, receiptFilename(env.TransferID, ReceiptDestinationMaildirCommit))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func assertApplyNotCommitted(t *testing.T, root string, env bridge.Envelope) {
	t.Helper()
	path := filepath.Join(root, "agents", "claude", "inbox", "new", bridge.TransferFilename(env.SourceHost, env.TransferID))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unexpected committed file %s: %v", path, err)
	}
	receipt := filepath.Join(root, applyReceiptRelDir, receiptFilename(env.TransferID, ReceiptDestinationMaildirCommit))
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatalf("unexpected receipt %s: %v", receipt, err)
	}
}
