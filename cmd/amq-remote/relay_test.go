package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	body, tag := enrollShare(t, keyDir, owner)
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
	wg := newRelaySet(root, stateDir, "", buildDMEdges(root, stateDir, mf.Relay, io.Discard), io.Discard)
	wg.start(ctx, mf.Relay)
	defer func() { cancel(); wg.wait() }()

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

// #1024: a running serve follows the manifest relay block. A share added
// connects without a restart, an edited share keeps its running values and
// says to restart, and a removed share stops.
func TestRelayShareFollowsTheManifest(t *testing.T) {
	root := t.TempDir()
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	body, tag := enrollShare(t, filepath.Join(root, "extensions", "remote", "keys", "work"), owner)
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()})
	defer srv.Close()
	stateDir := filepath.Join(root, "extensions", "remote")
	path := manifest.DefaultPath(stateDir)
	write := func(r *manifest.Relay) {
		t.Helper()
		if err := manifest.Write(path, manifest.File{SchemaVersion: manifest.RelaySchemaVersion, Adapters: []manifest.Adapter{{Kind: "fake", Target: "fake", Epoch: "e_1"}}, Relay: r}); err != nil {
			t.Fatal(err)
		}
	}
	write(nil)
	errs := newSyncBuffer()
	rs := newRelaySet(root, stateDir, path, buildDMEdges(root, stateDir, nil, errs), errs)
	ctx, cancel := context.WithCancel(context.Background())
	rs.start(ctx, nil)
	defer func() { cancel(); rs.wait() }()
	rs.tick()
	waitFor := func(what string, ok func(*relayStatusDoc) bool) {
		t.Helper()
		for deadline := time.Now().Add(4 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if doc, err := loadRelayStatus(stateDir); err == nil && ok(doc) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s; stderr: %s", what, errs.String())
			}
		}
	}

	share := manifest.Share{Target: "fake", Session: "work", OwnerPubKey: tag.OwnerPubKey}
	write(&manifest.Relay{URL: url, Shares: []manifest.Share{share}})
	rs.tick()
	waitFor("the added share never authenticated", func(doc *relayStatusDoc) bool {
		return len(doc.Shares) == 1 && doc.Shares[0].State == string(relay.StateAuthenticated) && lr.AuthOK.Load() == 1
	})

	edited := share
	edited.DMChannelID = "dm-2"
	write(&manifest.Relay{URL: url, Shares: []manifest.Share{edited}})
	rs.tick()
	if !strings.Contains(errs.String(), "relay share work: its manifest entry changed; restart serve, or remove and re-add the share, to adopt it") {
		t.Fatalf("stderr = %q, want the restart line", errs.String())
	}
	if got := rs.running["work"].share; got.DMChannelID != "" || lr.AuthOK.Load() != 1 {
		t.Fatalf("running share = %+v after %d auths, want the old values on the same connection", got, lr.AuthOK.Load())
	}

	write(nil) // the last share goes with its relay block
	rs.tick()
	waitFor("the removed share is still shown", func(doc *relayStatusDoc) bool { return len(doc.Shares) == 0 })
}

// codex slice 1 review r2 #1: replacing body.key and its complete
// generation with another body under the same owner was adopted on the next
// reconnect. The first body a serve loads is pinned until restart.
func TestRelayConfigRefusesReplacedBody(t *testing.T) {
	root := t.TempDir()
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "work")
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	_, tag := enrollShare(t, keyDir, owner)
	cfg := relayConfigFor(root, "ws://127.0.0.1:1", manifest.Share{Target: "fake", Session: "work", OwnerPubKey: tag.OwnerPubKey})
	if _, err := cfg(); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if err := os.RemoveAll(keyDir); err != nil {
		t.Fatal(err)
	}
	enrollShare(t, keyDir, owner)
	if _, err := cfg(); err == nil || !strings.Contains(err.Error(), "enrolled body changed") {
		t.Fatalf("replaced body: err=%v, want the pinned body enforced", err)
	}
}

// enrollShare mints a body key in keyDir and writes a complete owner-signed
// generation (every base kind, one not-after), returning the body and its
// AUTH-kind tag.
func enrollShare(t *testing.T, keyDir string, owner [32]byte) (*bodykey.BodyKey, *bodykey.AuthTag) {
	t.Helper()
	body, err := bodykey.Mint(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(time.Hour).Unix()
	var tags []map[string]any
	var auth *bodykey.AuthTag
	for _, kind := range bodykey.ShareKinds {
		tag, err := bodykey.SignAuthTag(owner, body.PublicKeyHex(), bodykey.ShareConditions(kind, notAfter))
		if err != nil {
			t.Fatal(err)
		}
		tags = append(tags, map[string]any{"kind": kind, "owner_pubkey": tag.OwnerPubKey, "conditions": tag.Conditions, "sig": tag.SigHex()})
		if kind == authKind {
			auth = tag
		}
	}
	raw, _ := json.Marshal(map[string]any{"tags": tags})
	if err := os.WriteFile(filepath.Join(keyDir, "share.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return body, auth
}
