package bridge

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSignAndVerifyEnvelopeRoundTrip(t *testing.T) {
	key, err := GenerateHostKey("1")
	if err != nil {
		t.Fatal(err)
	}
	env := testEnvelope([]byte("signed-payload"))
	if err := SignEnvelope(&env, key); err != nil {
		t.Fatal(err)
	}
	if env.Version != 2 {
		t.Fatalf("envelope version = %d, want v2", env.Version)
	}
	if err := VerifyEnvelope(env, key.Public(), "1"); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestV1CanonicalBytesHadLFFieldCollision(t *testing.T) {
	left := testEnvelope([]byte("legacy"))
	left.ThreadID = "thread\npayload_sha256=one"
	left.PayloadSHA256 = "two"
	right := left
	right.ThreadID = "thread"
	right.PayloadSHA256 = "one\npayload_sha256=two"

	if !bytes.Equal(legacyV1CanonicalBytes(left), legacyV1CanonicalBytes(right)) {
		t.Fatal("test setup did not reproduce the v1 LF collision")
	}
	if bytes.Equal(CanonicalBytes(left), CanonicalBytes(right)) {
		t.Fatal("v2 length-prefixed preimages still collide")
	}
}

func TestCanonicalBytesUsesLengthPrefixedV2Fields(t *testing.T) {
	env := testEnvelope([]byte("payload"))
	got := CanonicalBytes(env)
	want := []string{
		"amq-bridge-envelope-v2",
		"2",
		env.TransferID,
		env.SourceHost,
		env.SourceHandle,
		env.DestAlias,
		env.SourceMessageID,
		env.ThreadID,
		env.PayloadSHA256,
		env.KeyGeneration,
	}
	for i, wantField := range want {
		if len(got) < 4 {
			t.Fatalf("field %d has no length prefix", i)
		}
		length := int(binary.BigEndian.Uint32(got[:4]))
		got = got[4:]
		if length != len(wantField) || len(got) < length || string(got[:length]) != wantField {
			t.Fatalf("field %d = %q with length %d, want %q with length %d", i, got[:min(length, len(got))], length, wantField, len(wantField))
		}
		got = got[length:]
	}
	if len(got) != 0 {
		t.Fatalf("canonical preimage has %d trailing bytes", len(got))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func legacyV1CanonicalBytes(env Envelope) []byte {
	return []byte(strings.Join([]string{
		"amq-bridge-envelope-v1",
		"version=" + strconv.Itoa(env.Version),
		"transfer_id=" + env.TransferID,
		"source_host=" + env.SourceHost,
		"source_handle=" + env.SourceHandle,
		"dest_alias=" + env.DestAlias,
		"source_message_id=" + env.SourceMessageID,
		"thread_id=" + env.ThreadID,
		"payload_sha256=" + env.PayloadSHA256,
		"key_generation=" + env.KeyGeneration,
	}, "\n"))
}

func TestVerifyEnvelopeRejectsForgedSourceHost(t *testing.T) {
	honest, err := GenerateHostKey("1")
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := GenerateHostKey("1")
	if err != nil {
		t.Fatal(err)
	}
	env := testEnvelope([]byte("forged"))
	env.SourceHost = "grok-host"
	if err := SignEnvelope(&env, attacker); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnvelope(env, honest.Public(), "1"); err == nil {
		t.Fatal("forged source_host signature accepted")
	}
}

func TestVerifyEnvelopeRejectsGenerationMismatch(t *testing.T) {
	key, err := GenerateHostKey("1")
	if err != nil {
		t.Fatal(err)
	}
	env := testEnvelope([]byte("rotated"))
	if err := SignEnvelope(&env, key); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnvelope(env, key.Public(), "2"); err == nil {
		t.Fatal("revoked generation accepted")
	}
}

func TestLoadIdentityRefusesSymlinkAndPermissiveMode(t *testing.T) {
	root := t.TempDir()
	key, err := GenerateHostKey("1")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteHostID(root, "grok"); err != nil {
		t.Fatal(err)
	}
	if err := WriteIdentity(root, key); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(IdentityPath(root), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(root); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("permissive identity error = %v, want 0600 refusal", err)
	}
	if err := os.Chmod(IdentityPath(root), 0o600); err != nil {
		t.Fatal(err)
	}

	linkRoot := t.TempDir()
	if err := WriteHostID(linkRoot, "grok"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(linkRoot, "bridge"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(IdentityPath(root), IdentityPath(linkRoot)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := LoadIdentity(linkRoot); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink identity error = %v, want symlink refusal", err)
	}
}

func TestLoadTrustedRejectsUnknownHostKey(t *testing.T) {
	root := t.TempDir()
	if err := WriteHostID(root, "mac"); err != nil {
		t.Fatal(err)
	}
	pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
	if err := WriteTrusted(root, "grok", pub, "1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadTrusted(root, "other"); err == nil {
		t.Fatal("missing trusted host was accepted")
	}
	got, generation, err := LoadTrusted(root, "grok")
	if err != nil || generation != "1" || hex.EncodeToString(got) != hex.EncodeToString(pub) {
		t.Fatalf("trusted grok = %x %q %v", got, generation, err)
	}
}
