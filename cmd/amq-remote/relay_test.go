package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
)

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

// Review #1048 r1: a running serve never holds two shares on one target, a
// startup duplicate-body pair recovers once one of them is removed, and only
// a setup refusal is retried: a refusal a surface reports at run time does
// not bounce the share on an unrelated manifest rewrite.
func TestRelayReconcileKeepsOneShareAndRetriesOnlySetupRefusals(t *testing.T) {
	root := t.TempDir()
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(root, "extensions", "remote", "keys")
	body, tag := enrollShare(t, filepath.Join(keys, "work"), owner, buzzio.KindDM, buzzio.KindEdit)
	// work2 holds the same body: a copy of work's key directory.
	if err := os.CopyFS(filepath.Join(keys, "work2"), os.DirFS(filepath.Join(keys, "work"))); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(filepath.Join(keys, "work2"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o700)
		}
		return os.Chmod(p, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	_, srv, url := relaytest.Start(body.PublicKeyHex(), []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()})
	defer srv.Close()
	stateDir := filepath.Join(root, "extensions", "remote")
	path := manifest.DefaultPath(stateDir)
	epoch := "e_1"
	write := func(shares ...manifest.Share) *manifest.Relay {
		t.Helper()
		var ads []manifest.Adapter
		for _, target := range []string{"a", "b", "c", "d", "e"} {
			ads = append(ads, manifest.Adapter{Kind: "fake", Target: target, Epoch: epoch})
		}
		r := &manifest.Relay{URL: url, Shares: shares}
		if err := manifest.Write(path, manifest.File{SchemaVersion: manifest.RelaySchemaVersion, Adapters: ads, Relay: r}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	cmd := func(session, target string) manifest.Share {
		return manifest.Share{Target: target, Session: session, OwnerPubKey: tag.OwnerPubKey, Commands: true, DMChannelID: "dm-" + session, NativeSessionID: "native-" + session}
	}
	plain := manifest.Share{Target: "c", Session: "plain", OwnerPubKey: tag.OwnerPubKey}
	started := write(cmd("work", "a"), cmd("work2", "b"), plain)
	errs := newSyncBuffer()
	edges := buildDMEdges(root, stateDir, started, errs)
	rs := newRelaySet(root, stateDir, path, edges, errs)
	ctx, cancel := context.WithCancel(context.Background())
	rs.start(ctx, started)
	defer func() { cancel(); rs.wait() }()
	rs.tick()
	const dup = "body key is also enrolled"
	if !strings.Contains(edges.stateOf("work"), dup) || !strings.Contains(edges.stateOf("work2"), dup) {
		t.Fatalf("startup states = %q, %q; want both refused as duplicates", edges.stateOf("work"), edges.stateOf("work2"))
	}

	// The pair recovers: with work2 gone, work's retry no longer finds the
	// duplicate mark.
	write(cmd("work", "a"), plain)
	rs.tick()
	if strings.Contains(edges.stateOf("work"), dup) {
		t.Fatalf("work state = %q after its duplicate was removed", edges.stateOf("work"))
	}

	// A run-time refusal is not a setup refusal: an unrelated rewrite (an
	// adapter epoch, as persistAdapter writes) keeps the same client.
	edges.setState("plain", "refused: the target went away")
	before := rs.running["plain"]
	epoch = "e_2"
	write(cmd("work", "a"), plain)
	rs.tick()
	if rs.running["plain"] != before {
		t.Fatal("an unrelated manifest rewrite restarted a share with a run-time refusal")
	}

	// A hand edit that moves plain to e and adds thief on c: plain keeps c
	// until a restart, so thief is refused and c keeps one share.
	moved := plain
	moved.Target = "e"
	thief := manifest.Share{Target: "c", Session: "thief", OwnerPubKey: tag.OwnerPubKey}
	write(cmd("work", "a"), moved, thief)
	rs.tick()
	if rs.running["thief"] != nil {
		t.Fatal("a share was added on a target a running share still serves")
	}
	if !strings.Contains(errs.String(), "relay share thief: refused: running share plain still serves target c") {
		t.Fatalf("stderr = %q, want the refusal for thief", errs.String())
	}
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
func enrollShare(t *testing.T, keyDir string, owner [32]byte, extraKinds ...uint16) (*bodykey.BodyKey, *bodykey.AuthTag) {
	t.Helper()
	body, err := bodykey.Mint(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(time.Hour).Unix()
	var tags []map[string]any
	var auth *bodykey.AuthTag
	for _, kind := range append(append([]uint16{}, bodykey.ShareKinds...), extraKinds...) {
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
