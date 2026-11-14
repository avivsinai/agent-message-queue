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
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
)

// 611.30: one run shows the body and grants, reads the key only after the
// owner confirms, signs every grant a `share` output printed into one bundle,
// and then publishes the owner's kind 30177 policy for the body.
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
	keyReads := 0
	readKey := func() ([32]byte, error) { keyReads++; return owner, nil }
	bundlePath := filepath.Join(dir, "bundle.json")
	var published nostr.Event
	publish := func(_ context.Context, url string, evt nostr.Event, _ [32]byte) error {
		if _, err := os.Stat(bundlePath); err != nil {
			t.Errorf("policy published before the bundle was written: %v", err)
		}
		if url != "wss://relay.example" {
			t.Errorf("published to %s", url)
		}
		published = evt
		return nil
	}
	o := options{share: sharePath, out: bundlePath, relay: "wss://relay.example", name: "AMQ session"}

	// A refused confirmation reads no key and writes nothing.
	var summary bytes.Buffer
	confirmed := ""
	refuse := func(body string) (bool, error) { confirmed = body; return false, nil }
	if err := run(o, refuse, readKey, publish, &summary); err == nil || keyReads != 0 {
		t.Fatalf("refused confirmation: err=%v keyReads=%d", err, keyReads)
	}
	if confirmed != body.PublicKeyHex() || !bytes.Contains(summary.Bytes(), []byte("body pubkey: "+body.PublicKeyHex())) {
		t.Fatalf("summary does not name the body:\n%s", summary.String())
	}
	if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
		t.Fatalf("bundle written without confirmation: %v", err)
	}

	accept := func(string) (bool, error) { return true, nil }
	if err := run(o, accept, readKey, publish, io.Discard); err != nil || keyReads != 1 {
		t.Fatalf("run: err=%v keyReads=%d", err, keyReads)
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

// Machine mode signs the one grant a `machine request` printed: the written
// grant verifies against the agent key and carries the request's conditions,
// and the policy names the agent.
func TestSignsMachineRequestIntoOneGrant(t *testing.T) {
	dir := t.TempDir()
	body, err := machinecred.Mint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(90 * 24 * time.Hour).Unix()
	reqPath := filepath.Join(dir, "request.txt")
	if err := os.WriteFile(reqPath, []byte(machinecred.Request(body, "AMQ Remote @ host", notAfter)), 0o600); err != nil {
		t.Fatal(err)
	}
	var owner [32]byte
	owner[31] = 7
	readKey := func() ([32]byte, error) { return owner, nil }
	confirmed := ""
	accept := func(b string) (bool, error) { confirmed = b; return true, nil }
	var published nostr.Event
	publish := func(_ context.Context, _ string, evt nostr.Event, _ [32]byte) error { published = evt; return nil }
	grantPath := filepath.Join(dir, "grant.json")
	o := options{machine: reqPath, out: grantPath, relay: "wss://relay.example", name: "AMQ session"}

	var stdout bytes.Buffer
	if err := run(o, accept, readKey, publish, &stdout); err != nil {
		t.Fatal(err)
	}
	t.Logf("stdout:\n%s", stdout.String())
	if confirmed != body {
		t.Fatalf("confirmed %q, want the body %s", confirmed, body)
	}

	raw, err := os.ReadFile(grantPath)
	if err != nil {
		t.Fatal(err)
	}
	var g machinecred.Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	tag := bodykey.AuthTag{OwnerPubKey: g.OwnerPubKey, Conditions: g.Conditions}
	if err := tag.SetSigHex(g.Sig); err != nil {
		t.Fatal(err)
	}
	if g.BodyPubKey != body || g.Conditions != bodykey.MachineConditions(notAfter) || g.OwnerPubKey != nostr.GetPublicKey(owner).Hex() || tag.Verify(body) != nil {
		t.Fatalf("grant = %s does not verify for the body with the request's conditions", raw)
	}
	if published.Kind != 30177 || published.Tags.GetD() != body || !bytes.Contains([]byte(published.Content), []byte(`"name":"AMQ Remote @ host"`)) {
		t.Fatalf("policy event = %+v", published)
	}
}
