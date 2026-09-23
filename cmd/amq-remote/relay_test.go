package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
)

// 611.15 slice 1: an enrolled share on disk (body.key + owner-signed
// share.json) and a relay-capable manifest make serve's relay client
// authenticate with the enrolled 1059 tag, and relay-status.json says so.
func TestRelayShareAuthenticatesFromEnrolledCredentials(t *testing.T) {
	root := t.TempDir()
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "work")
	body, err := bodykey.Mint(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	// A complete enrolled generation: every base kind, one not-after.
	notAfter := time.Now().Add(time.Hour).Unix()
	var tags []map[string]any
	var tag *bodykey.AuthTag
	for _, kind := range bodykey.ShareKinds {
		t2, err := bodykey.SignAuthTag(owner, body.PublicKeyHex(), bodykey.ShareConditions(kind, notAfter))
		if err != nil {
			t.Fatal(err)
		}
		tags = append(tags, map[string]any{"kind": kind, "owner_pubkey": t2.OwnerPubKey, "conditions": t2.Conditions, "sig": t2.SigHex()})
		if kind == authKind {
			tag = t2
		}
	}
	share, _ := json.Marshal(map[string]any{"tags": tags})
	if err := os.WriteFile(filepath.Join(keyDir, "share.json"), share, 0o600); err != nil {
		t.Fatal(err)
	}
	wire := []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()}
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), wire)
	defer srv.Close()

	mf := manifest.File{
		SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
		Adapters: []manifest.Adapter{{Kind: "fake", Target: "fake", Epoch: "e_1"}},
		Relay:    &manifest.Relay{URL: url, Shares: []manifest.Share{{Target: "fake", Session: "work", OwnerPubKey: tag.OwnerPubKey}}},
	}
	if err := manifest.Validate(mf); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")
	ctx, cancel := context.WithCancel(context.Background())
	wg := startRelays(ctx, root, stateDir, mf.Relay, nil, io.Discard)
	defer func() { cancel(); wg.Wait() }()

	deadline := time.Now().Add(4 * time.Second)
	for {
		doc, err := loadRelayStatus(stateDir)
		if err == nil && len(doc.Shares) == 1 && doc.Shares[0].State == string(relay.StateAuthenticated) && lr.AuthOK.Load() >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay status = %+v err=%v, want the share authenticated", doc, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
