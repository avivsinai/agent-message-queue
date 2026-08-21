package bridge

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
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
	if err := VerifyEnvelope(env, key.Public(), "1"); err != nil {
		t.Fatalf("verify: %v", err)
	}
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
