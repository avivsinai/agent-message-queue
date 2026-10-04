package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Bead 611.42.4: the hook pin names only the owner and the AMQ root. The
// allow config reads the share serving the Claude session from that root's
// manifest and enrolled credentials, and verifies the owner's ✅ on that
// share's approval message, signing in to the share's relay as serve does
// to read the message's history.
func TestAllowConfigVerifiesOnThePinnedShare(t *testing.T) {
	root := t.TempDir()
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	body, tag := enrollShare(t, filepath.Join(root, "extensions", "remote", "keys", "work"), owner)
	_, srv, url := relaytest.Start(body.PublicKeyHex(), []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()})
	defer srv.Close()
	ownerHex := nostr.GetPublicKey(owner).Hex()
	if err := manifest.Write(manifest.DefaultPath(filepath.Join(root, stateDirName)), manifest.File{
		SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
		Adapters: []manifest.Adapter{{Kind: "fake", Target: "fake", Epoch: "e_1"}},
		Relay: &manifest.Relay{URL: url, Shares: []manifest.Share{{Target: "fake", Session: "work", OwnerPubKey: ownerHex,
			DMChannelID: "dm-1", NativeSessionID: "sess-1", Commands: true}}},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := allowConfig(claude.HookPin{Owner: ownerHex, Root: root})
	share, err := cfg.Share("sess-1")
	if err != nil || share.Body != body.PublicKeyHex() || share.Channel != "dm-1" || share.Target != "fake" {
		t.Fatalf("share = %+v (%v), want the enrolled body, its DM channel and target", share, err)
	}
	prompt := "Bash command:\ngo test ./...\n\nInteraction: cc-" + hex32() + "\nAction: sha256:" + hex32() + hex32()
	ref := protocol.EncodeRef("buzz-host", "fake", "123e4567-e89b-12d3-a456-426614174000")
	msg := nostr.Event{CreatedAt: nostr.Now(), Kind: 9, Tags: nostr.Tags{{"h", "dm-1"}},
		Content: "Approval needed (" + ref + "):\n\n" + prompt + "\n\nReact ✅ to approve or ❌ to reject. The first answer, here or in the terminal, wins."}
	if err := msg.Sign(body.Secret()); err != nil {
		t.Fatal(err)
	}
	react := nostr.Event{CreatedAt: nostr.Now(), Kind: 7, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	evidence, _ := json.Marshal(map[string]any{"reaction": react, "message": msg})
	want := claude.AllowCheck{Share: share, Prompt: prompt, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cfg.Verify(ctx, evidence, want); err != nil {
		t.Fatalf("verify = %v, want the owner's ✅ proven", err)
	}
}

func hex32() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
