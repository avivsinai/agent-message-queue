package xferv3

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyAndConsumeBindsReceiptToItem(t *testing.T) {
	seed := bytes.Repeat([]byte{0x71}, 32)
	key := ed25519.NewKeyFromSeed(seed)
	receiver := mustHost(t, "box")
	sender := mustHost(t, "mac")
	var want ID
	var other ID
	copy(want.sum[:], bytes.Repeat([]byte{0x21}, 32))
	copy(other.sum[:], bytes.Repeat([]byte{0x22}, 32))
	dir := t.TempDir()
	path := filepath.Join(dir, want.String())
	if err := os.WriteFile(path, []byte("owed"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrong, err := NewReceipt(receiver, sender, other)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignReceipt(key, wrong)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAndConsume(path, key.Public().(ed25519.PublicKey), signed, receiver, sender, want); !errors.Is(err, ErrBadID) {
		t.Fatalf("mismatched receipt id: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("consumed on a mismatched receipt: %v", err)
	}
	right, err := NewReceipt(receiver, sender, want)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := SignReceipt(key, right)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAndConsume(path, key.Public().(ed25519.PublicKey), ok, receiver, sender, want); err != nil {
		t.Fatalf("matching receipt: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("path remains: %v", err)
	}
}
