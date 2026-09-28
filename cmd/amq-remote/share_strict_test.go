package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// 611.30: the strict relay share in one owner step. share prints the
// window, the owner signs it into one bundle, and one share --bundle
// command enrolls it and writes the relay block with the DM binding. serve
// then authenticates and opens the DM surface, and doctor reports no
// relay_auth, dm_surface, or publication failure.
func TestStrictShareBundleEnrollsAndDoctorIsClean(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "extensions", "remote")
	manifestPath := manifest.DefaultPath(stateDir)
	if err := manifest.Write(manifestPath, manifest.File{
		SchemaVersion: manifest.SchemaVersion, Layer: manifest.Layer,
		Adapters: []manifest.Adapter{{Kind: "fake", Target: "fake", Epoch: "e_1"}},
	}); err != nil {
		t.Fatal(err)
	}
	out, _, _ := runShare(t, "--root", root, "--session", "work", "--enable", "buzz-dm")
	body, err := bodykey.Load(filepath.Join(stateDir, "keys", "work", "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	var tags []shareTagFile
	var wire []string
	for kind, conds := range pendingConditions(t, out) {
		tf := shareTagFile{Kind: kind, OwnerPubKey: ownerPubHex, Conditions: conds, Sig: ownerSignFor(t, body.PublicKeyHex(), conds)}
		tags = append(tags, tf)
		if kind == authKind {
			wire = []string{"auth", tf.OwnerPubKey, tf.Conditions, tf.Sig}
		}
	}
	raw, _ := json.Marshal(tags)
	bundle := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(bundle, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), wire)
	defer srv.Close()

	runShare(t, "--root", root, "--session", "work", "--bundle", bundle, "--target", "fake", "--relay", url,
		"--dm-channel", "dm-1", "--native-session", "thread-1")

	mf, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	want := manifest.Share{Target: "fake", Session: "work", OwnerPubKey: ownerPubHex, DMChannelID: "dm-1", NativeSessionID: "thread-1", Commands: true}
	if mf.Relay == nil || mf.Relay.URL != url || len(mf.Relay.Shares) != 1 || mf.Relay.Shares[0].Target != want.Target ||
		mf.Relay.Shares[0].DMChannelID != want.DMChannelID || mf.Relay.Shares[0].NativeSessionID != want.NativeSessionID || !mf.Relay.Shares[0].Commands {
		t.Fatalf("relay block = %+v, want share %+v", mf.Relay, want)
	}

	// The loopback relay serves no NIP-11 document, so pin its self key the
	// way relay_self does; the relay signs the DM's membership and metadata.
	relayKey := nostr.Generate()
	mf.Relay.Self = nostr.GetPublicKey(relayKey).Hex()
	for _, g := range []struct {
		kind nostr.Kind
		tags nostr.Tags
	}{
		{39000, nostr.Tags{{"private"}, {"t", "dm"}}},
		{39002, nostr.Tags{{"p", ownerPubHex}, {"p", body.PublicKeyHex()}}},
	} {
		evt := nostr.Event{CreatedAt: nostr.Now(), Kind: g.kind, Tags: append(nostr.Tags{{"d", "dm-1"}}, g.tags...)}
		if err := evt.Sign(relayKey); err != nil {
			t.Fatal(err)
		}
		lr.Inject(evt)
	}

	edges := buildDMEdges(root, stateDir, mf.Relay, io.Discard)
	edges.bind(func(cmd *protocol.Command, _ core.Source) (any, error) {
		return protocol.Session{TargetID: "fake", Epoch: "e_1"}, nil
	}, func(string) string { return "thread-1" })
	ctx, cancel := context.WithCancel(context.Background())
	wg := startRelays(ctx, root, stateDir, mf.Relay, edges, io.Discard)
	defer func() { cancel(); wg.Wait() }()

	deadline := time.Now().Add(4 * time.Second)
	for {
		doc, err := loadRelayStatus(stateDir)
		if err == nil && len(doc.Shares) == 1 && doc.Shares[0].State == string(relay.StateAuthenticated) && doc.Shares[0].Commands == "subscription_active" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay status = %+v err=%v, want authenticated with the DM surface open", doc, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	report, _, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.(map[string]any)["failing"].([]boundaryFailure) {
		switch f.Boundary {
		case "relay_auth", "dm_surface", "publication", "body_key", "tag_expiry":
			t.Fatalf("doctor fails %s: %s", f.Boundary, f.Detail)
		}
	}
}
