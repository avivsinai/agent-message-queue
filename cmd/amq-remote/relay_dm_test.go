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

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// 611.16 slice 4, through serve's relay wiring: an owner DM in the bound
// channel reaches the endpoint as a submit, and the body publishes the
// request's result row back into that channel.
func TestDMEdgeSubmitsOwnerMessageAndPublishesRow(t *testing.T) {
	prev := verifyMembership
	verifyMembership = func(context.Context, *relay.Conn, buzzio.Binding) error { return nil } // contract pending; see relay_dm.go
	defer func() { verifyMembership = prev }()

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
	for _, kind := range []uint16{authKind, buzzio.KindDM, buzzio.KindEdit} {
		tag, err := bodykey.SignAuthTag(owner, body.PublicKeyHex(), bodykey.ShareConditions(kind, notAfter))
		if err != nil {
			t.Fatal(err)
		}
		tags = append(tags, map[string]any{"kind": kind, "owner_pubkey": tag.OwnerPubKey, "conditions": tag.Conditions, "sig": tag.SigHex()})
		if kind == authKind {
			authTag = []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()}
		}
	}
	share, _ := json.Marshal(map[string]any{"tags": tags})
	if err := os.WriteFile(filepath.Join(keyDir, "share.json"), share, 0o600); err != nil {
		t.Fatal(err)
	}
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), authTag)
	defer srv.Close()
	ownerHex := nostr.GetPublicKey(owner).Hex()
	r := &manifest.Relay{URL: url, Shares: []manifest.Share{{Target: "fake", Session: "work", OwnerPubKey: ownerHex, DMChannelID: "dm-1", Commands: true}}}

	stateDir := filepath.Join(root, "extensions", "remote")
	edges := buildDMEdges(root, stateDir, r, io.Discard)
	submitted := make(chan string, 1)
	edges.bind(func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			return protocol.Session{TargetID: "fake", Epoch: "e_1"}, nil
		case protocol.OpRequestSubmit:
			submitted <- cmd.Input.Text
			return protocol.Reply{Snapshot: protocol.Snapshot{RequestRef: "amqr1_dm", Revision: 1, State: protocol.StateRunning}}, nil
		}
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	wg := startRelays(ctx, root, stateDir, r, edges, io.Discard)
	defer func() { cancel(); wg.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for edges.stateOf("work") != "subscription_active" {
		if time.Now().After(deadline) {
			t.Fatalf("DM surface state = %q, want subscription_active", edges.stateOf("work"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	dm := nostr.Event{CreatedAt: nostr.Now(), Kind: buzzio.KindDM, Content: "fix the build", Tags: nostr.Tags{{"h", "dm-1"}}}
	if err := dm.Sign(owner); err != nil {
		t.Fatal(err)
	}
	lr.Inject(dm)
	select {
	case text := <-submitted:
		if text != "fix the build" {
			t.Fatalf("submitted %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner DM never reached the endpoint")
	}
	for {
		for _, evt := range lr.Events() {
			if evt.Kind == buzzio.KindDM && evt.PubKey.Hex() == body.PublicKeyHex() {
				return // the body's result row is on the relay
			}
		}
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("no result row from the body on the relay")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
