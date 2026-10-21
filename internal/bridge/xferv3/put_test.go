package xferv3

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestPutTxWritesIdempotentBundle(t *testing.T) {
	root := t.TempDir()
	host, err := ParseHost("box")
	if err != nil {
		t.Fatal(err)
	}
	allow, err := ParseHandle("grok")
	if err != nil {
		t.Fatal(err)
	}
	var xPublic [32]byte
	peer, err := NewPeer(host, []Handle{allow}, "", xPublic)
	if err != nil {
		t.Fatal(err)
	}
	if err := StorePeer(root, peer); err != nil {
		t.Fatal(err)
	}
	source, err := ParseHost("mac")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := ParseHandle("codex")
	if err != nil {
		t.Fatal(err)
	}
	dest, err := ParseDest("box/grok")
	if err != nil {
		t.Fatal(err)
	}
	message, err := ParseMessageID("m1")
	if err != nil {
		t.Fatal(err)
	}
	thread, err := ParseThreadID("p2p/codex__grok")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("ping")
	rec, err := NewRecord(source, handle, dest, message, thread, payload)
	if err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x11}, 32))
	id, err := PutTx(root, priv, rec, payload)
	if err != nil {
		t.Fatal(err)
	}
	if id.String() != rec.ID().String() {
		t.Fatalf("id = %s, want %s", id, rec.ID())
	}
	path := filepath.Join(root, "bridge", "tx", "box", id.String())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signed, gotPayload, err := ParseBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload = %q, want %q", gotPayload, payload)
	}
	if err := Verify(priv.Public().(ed25519.PublicKey), signed); err != nil {
		t.Fatal(err)
	}
	again, err := PutTx(root, priv, rec, payload)
	if err != nil {
		t.Fatal(err)
	}
	if again.String() != id.String() {
		t.Fatalf("retry id = %s, want %s", again, id)
	}
	entries, err := os.ReadDir(filepath.Join(root, "bridge", "tx", "box"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("tx directory holds %d entries, want only the bundle", len(entries))
	}
}
