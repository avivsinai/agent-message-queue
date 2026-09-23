package sharestate

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// enroll writes a body key and an owner-signed generation for kinds under
// root's keys/s directory.
func enroll(t *testing.T, root string, kinds []uint16) {
	t.Helper()
	dir := filepath.Join(root, "extensions", "remote", "keys", "s")
	body, err := bodykey.Mint(dir)
	if err != nil {
		t.Fatal(err)
	}
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(time.Hour).Unix()
	var tags []map[string]any
	for _, k := range kinds {
		tag, err := bodykey.SignAuthTag(owner, body.PublicKeyHex(), bodykey.ShareConditions(k, notAfter))
		if err != nil {
			t.Fatal(err)
		}
		tags = append(tags, map[string]any{"kind": k, "owner_pubkey": tag.OwnerPubKey, "conditions": tag.Conditions, "sig": tag.SigHex()})
	}
	raw, _ := json.Marshal(map[string]any{"tags": tags})
	if err := os.WriteFile(filepath.Join(dir, "share.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// codex slice 1 review #3: a generation holding only the 1059 grant
// authenticated; a partial generation is not one share produced.
func TestLoadRefusesPartialGeneration(t *testing.T) {
	root := t.TempDir()
	enroll(t, root, []uint16{1059})
	if _, err := Load(root, "s"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("partial generation: err=%v, want ErrNotEnrolled", err)
	}
}

// codex slice 1 review #4: a symlinked keys directory adopted a body key
// and generation from outside the root.
func TestLoadRefusesSymlinkedKeysDir(t *testing.T) {
	outside := t.TempDir()
	enroll(t, outside, bodykey.ShareKinds)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "extensions", "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "extensions", "remote", "keys"), filepath.Join(root, "extensions", "remote", "keys")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := Load(outside, "s"); err != nil {
		t.Fatalf("control: the real directory must load: %v", err)
	}
	if _, err := Load(root, "s"); err == nil {
		t.Fatal("loaded a generation through a symlinked keys directory")
	}
}
