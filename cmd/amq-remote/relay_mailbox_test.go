package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// An owner DM to a strict share of a mailbox target reaches the handle as
// one AMQ message from buzz labeled with the DM event, and the handle's
// `amq reply` becomes the text of the request's result row.
func TestDMToMailboxTargetDeliversMessageAndPublishesReply(t *testing.T) {
	root := t.TempDir()
	for _, box := range []string{"new", "cur", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, "agents", "squad", "inbox", box), 0o700); err != nil {
			t.Fatal(err)
		}
	}
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
	for _, kind := range append(append([]uint16{}, bodykey.ShareKinds...), buzzio.KindDM, buzzio.KindEdit) {
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
	relayKey := nostr.Generate()
	for _, evt := range []nostr.Event{
		{CreatedAt: nostr.Now(), Kind: 39000, Tags: nostr.Tags{{"d", "dm-1"}, {"private"}, {"t", "dm"}}},
		{CreatedAt: nostr.Now(), Kind: 39002, Tags: nostr.Tags{{"d", "dm-1"}, {"p", ownerHex}, {"p", body.PublicKeyHex()}}},
	} {
		if err := evt.Sign(relayKey); err != nil {
			t.Fatal(err)
		}
		lr.Inject(evt)
	}
	r := &manifest.Relay{URL: url, Self: nostr.GetPublicKey(relayKey).Hex(), Shares: []manifest.Share{{
		Target: "squad", Session: "work", OwnerPubKey: ownerHex, DMChannelID: "dm-1",
		NativeSessionID: "squad", MinEvidence: "submitted", Commands: true,
	}}}

	stateDir := filepath.Join(root, "extensions", "remote")
	built := registry.Build(context.Background(), root, stateDir, "remote", manifest.File{Adapters: []manifest.Adapter{
		{Kind: "mailbox", Target: "squad", Config: json.RawMessage(`{"handle":"squad"}`)},
	}})
	if len(built) != 1 || built[0].Refusal != nil {
		t.Fatalf("mailbox adapter did not build: %+v", built)
	}
	store, ep, err := openServeStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ep.Close(); _ = store.Close() }()
	edges := buildDMEdges(root, stateDir, r, io.Discard)
	ep.SetPublish(publishRouter(map[string]publishFunc{"buzz": edges.publish}))
	ep.Register(built[0].Attachment)
	edges.bind(ep.Handle, ep.NativeSessionID)
	edges.bindAttachments(ep.AttachmentOf)
	ctx, cancel := context.WithCancel(context.Background())
	wg := newRelaySet(root, stateDir, "", edges, io.Discard)
	wg.start(ctx, r)
	defer func() { cancel(); wg.wait() }()

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

	inbox := filepath.Join(root, "agents", "squad", "inbox", "new")
	var prompt format.Message
	deadline = time.Now().Add(5 * time.Second)
	for {
		if names, _ := os.ReadDir(inbox); len(names) > 0 {
			if len(names) != 1 {
				t.Fatalf("handle inbox holds %d messages, want one", len(names))
			}
			if prompt, err = format.ReadMessageFile(filepath.Join(inbox, names[0].Name())); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner DM never reached the handle's inbox")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if prompt.Header.From != "buzz" || !slices.Contains(prompt.Header.Labels, "nostr:"+dm.ID.Hex()) || !strings.HasPrefix(prompt.Body, "fix the build") {
		t.Fatalf("prompt = %+v, want a message from buzz labeled nostr:%s", prompt, dm.ID.Hex())
	}

	// The handle answers with `amq reply --id <prompt>`.
	now := time.Now()
	replyID, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	reply := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: replyID, From: "squad", To: []string{"buzz"}, Thread: prompt.Header.Thread,
		Refs: []string{prompt.Header.ID}, Created: now.UTC().Format(time.RFC3339Nano), Kind: string(format.KindAnswer),
	}, Body: "build fixed in 3f2a"}
	data, err := reply.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "buzz", "inbox", "new", replyID+".md"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		if err := ep.Tick(); err != nil {
			t.Fatal(err)
		}
		for _, evt := range lr.Events() {
			if evt.Kind == buzzio.KindEdit && evt.PubKey.Hex() == body.PublicKeyHex() && strings.Contains(evt.Content, "build fixed in 3f2a") {
				return // the result row now shows the handle's reply
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no result row edit carries the handle's reply")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
