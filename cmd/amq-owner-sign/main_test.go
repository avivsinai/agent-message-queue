package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// 611.30: one run signs every grant a `share` output printed into one
// bundle and publishes the owner's kind 30177 policy for the body.
func TestSignsShareOutputIntoOneBundle(t *testing.T) {
	dir := t.TempDir()
	body, err := bodykey.Mint(filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(24 * time.Hour).Unix()
	kinds := append(append([]uint16{}, bodykey.ShareKinds...), 9, 40003)
	var out bytes.Buffer
	fmt.Fprintf(&out, "session:     work\nbody-pubkey: %s\n", body.PublicKeyHex())
	for _, kind := range kinds {
		conds := bodykey.ShareConditions(kind, notAfter)
		fmt.Fprintf(&out, "kind %d preimage: %s\nkind %d conditions: %s\n", kind, body.PreimageHex(conds), kind, conds)
	}
	sharePath := filepath.Join(dir, "share.txt")
	if err := os.WriteFile(sharePath, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var owner [32]byte
	owner[31] = 7
	var published nostr.Event
	publish := func(_ context.Context, url string, evt nostr.Event, _ [32]byte) error {
		if url != "wss://relay.example" {
			t.Errorf("published to %s", url)
		}
		published = evt
		return nil
	}
	bundlePath := filepath.Join(dir, "bundle.json")
	o := options{share: sharePath, out: bundlePath, relay: "wss://relay.example", name: "AMQ session"}
	if err := run(o, func() ([32]byte, error) { return owner, nil }, publish, io.Discard); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(bundlePath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bundle stat = %v, %v; want mode 0600", fi, err)
	}
	raw, _ := os.ReadFile(bundlePath)
	var tags []bundleTag
	if err := json.Unmarshal(raw, &tags); err != nil || len(tags) != len(kinds) {
		t.Fatalf("bundle = %s (%v), want %d tags", raw, err, len(kinds))
	}
	ownerHex := nostr.GetPublicKey(owner).Hex()
	for i, tf := range tags {
		tag := bodykey.AuthTag{OwnerPubKey: tf.OwnerPubKey, Conditions: tf.Conditions}
		if err := tag.SetSigHex(tf.Sig); err != nil {
			t.Fatal(err)
		}
		if tf.Kind != kinds[i] || tf.OwnerPubKey != ownerHex || tag.Verify(body.PublicKeyHex()) != nil {
			t.Fatalf("tag %d = %+v does not verify for the body", i, tf)
		}
	}
	if published.Kind != 30177 || published.Tags.GetD() != body.PublicKeyHex() || published.PubKey.Hex() != ownerHex || !published.VerifySignature() {
		t.Fatalf("policy event = %+v", published)
	}
}
