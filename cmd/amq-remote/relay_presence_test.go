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

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// 611.17 slice 2: a presence share publishes the body's kind 0 profile with
// the owner's one kind 0 grant and a kind 10100 "online" while the target
// is attached, finds the owner's 30177 policy, and publishes "offline" at a
// graceful shutdown.
func TestPresencePublishesProfileStatusAndOffline(t *testing.T) {
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
	notAfter := time.Now().Add(time.Hour).Unix()
	var tags []map[string]any
	var authTag []string
	for _, kind := range append(append([]uint16{}, bodykey.ShareKinds...), buzzio.KindProfile) {
		tag, err := bodykey.SignAuthTag(owner, body.PublicKeyHex(), bodykey.ShareConditions(kind, notAfter))
		if err != nil {
			t.Fatal(err)
		}
		tags = append(tags, map[string]any{"kind": kind, "owner_pubkey": tag.OwnerPubKey, "conditions": tag.Conditions, "sig": tag.SigHex()})
		if kind == authKind {
			authTag = []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()}
		}
	}
	raw, _ := json.Marshal(map[string]any{"tags": tags})
	if err := os.WriteFile(filepath.Join(keyDir, "share.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), authTag)
	defer srv.Close()
	// The owner's own client publishes the policy coordinate for this body.
	policy := nostr.Event{CreatedAt: nostr.Now(), Kind: buzzio.KindManagedAgent, Content: `{"name":"AMQ session","parallelism":1,"respond_to":"owner-only"}`, Tags: nostr.Tags{{"d", body.PublicKeyHex()}}}
	if err := policy.Sign(owner); err != nil {
		t.Fatal(err)
	}
	lr.Inject(policy)

	r := &manifest.Relay{URL: url, Shares: []manifest.Share{{Target: "fake", Session: "work", OwnerPubKey: nostr.GetPublicKey(owner).Hex(), Presence: true, Name: "AMQ work"}}}
	stateDir := filepath.Join(root, "extensions", "remote")
	edges := buildDMEdges(root, stateDir, r, io.Discard)
	edges.bind(func(cmd *protocol.Command, _ core.Source) (any, error) {
		return protocol.Session{TargetID: "fake", Attachment: "live"}, nil
	}, func(string) string { return "" })
	ctx, cancel := context.WithCancel(context.Background())
	wg := startRelays(ctx, root, stateDir, r, edges, io.Discard)

	deadline := time.Now().Add(4 * time.Second)
	for {
		if st, disc := edges.presenceView("work"); st == buzzio.StatusOnline && disc == "policy_present" {
			break
		}
		if time.Now().After(deadline) {
			st, disc := edges.presenceView("work")
			t.Fatalf("presence = %q discovery = %q, want online and policy_present", st, disc)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	var profileOK bool
	var statuses []string
	for _, evt := range lr.Events() {
		if evt.PubKey.Hex() != body.PublicKeyHex() {
			continue
		}
		auths := 0
		for _, tag := range evt.Tags {
			if len(tag) > 0 && tag[0] == "auth" {
				auths++
				if evt.Kind == buzzio.KindProfile && strings.HasPrefix(tag[2], "kind=0&") {
					profileOK = true
				}
			}
		}
		if auths != 1 {
			t.Fatalf("kind %d carries %d auth tags, want exactly one", evt.Kind, auths)
		}
		if evt.Kind == buzzio.KindAgentProfile {
			var c struct{ Status string }
			_ = json.Unmarshal([]byte(evt.Content), &c)
			statuses = append(statuses, c.Status)
		}
	}
	if !profileOK {
		t.Fatal("no body kind 0 profile carrying the owner's kind 0 grant")
	}
	if len(statuses) < 2 || statuses[0] != buzzio.StatusOnline || statuses[len(statuses)-1] != buzzio.StatusOffline {
		t.Fatalf("10100 statuses = %v, want online first and offline last", statuses)
	}
}
