package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Bead 611.42.4: install-approval-hook pins the share's relay, body, DM
// channel and target from the manifest and the enrolled credentials, and
// the allow config verifies the owner's ✅ on that share's approval
// message, signing in to the pinned relay as serve does to read its
// history.
//
// Pro review of #936 r2, P1: the hook read the relay and the share from the
// manifest, which a same-user process can rewrite. After the pin, a
// manifest rewritten to another relay (one that hides the edit) and another
// channel changes nothing: the pinned relay's edit still refuses.
func TestAllowConfigVerifiesOnThePinnedShare(t *testing.T) {
	root := t.TempDir()
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	body, tag := enrollShare(t, filepath.Join(root, "extensions", "remote", "keys", "work"), owner)
	wire := []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()}
	honest, srv, url := relaytest.Start(body.PublicKeyHex(), wire)
	defer srv.Close()
	_, other, otherURL := relaytest.Start(body.PublicKeyHex(), wire)
	defer other.Close()
	ownerHex := nostr.GetPublicKey(owner).Hex()
	writeManifest := func(relayURL, channel string) {
		t.Helper()
		if err := manifest.Write(manifest.DefaultPath(filepath.Join(root, stateDirName)), manifest.File{
			SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
			Adapters: []manifest.Adapter{{Kind: "fake", Target: "fake", Epoch: "e_1"}},
			Relay: &manifest.Relay{URL: relayURL, Shares: []manifest.Share{{Target: "fake", Session: "work", OwnerPubKey: ownerHex,
				DMChannelID: channel, NativeSessionID: "sess-1", Commands: true}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(url, "dm-1")
	pin, err := sharePin(ownerHex, root, "", "")
	if err != nil || pin.Relay != url || pin.Body != body.PublicKeyHex() || pin.Channel != "dm-1" || pin.Target != "fake" || !pin.Complete() {
		t.Fatalf("pin = %+v (%v), want the share's relay, enrolled body, DM channel and target", pin, err)
	}
	cfg := allowConfig(pin)
	share, err := cfg.Share("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Bash command:\ngo test ./...\n\nInteraction: cc-" + hex32() + "\nAction: sha256:" + hex32() + hex32()
	ref := protocol.EncodeRef("buzz-host", "fake", "123e4567-e89b-12d3-a456-426614174000")
	content := "Approval needed (" + ref + "):\n\n" + prompt + "\n\nReact ✅ or reply yes to approve; react ❌ or reply no to reject. The first answer, here or in the terminal, wins."
	msg := nostr.Event{CreatedAt: nostr.Now(), Kind: 9, Tags: nostr.Tags{{"h", "dm-1"}}, Content: content}
	if err := msg.Sign(body.Secret()); err != nil {
		t.Fatal(err)
	}
	react := nostr.Event{CreatedAt: nostr.Now(), Kind: 7, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	evidence, _ := json.Marshal(map[string]any{"reaction": react, "message": msg})
	want := claude.AllowCheck{Share: share, Prompt: prompt, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Minute)}
	verify := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return cfg.Verify(ctx, evidence, want)
	}
	if err := verify(); err != nil {
		t.Fatalf("verify = %v, want the owner's ✅ proven", err)
	}

	edit := nostr.Event{CreatedAt: msg.CreatedAt + 1, Kind: 40003, Tags: nostr.Tags{{"h", "dm-1"}, {"e", msg.ID.Hex()}},
		Content: strings.Replace(content, "go test ./...", "go vet ./...", 1)}
	if err := edit.Sign(body.Secret()); err != nil {
		t.Fatal(err)
	}
	honest.Inject(edit)
	writeManifest(otherURL, "dm-2")
	if err := verify(); !errors.Is(err, claude.ErrAllowAltered) {
		t.Fatalf("verify after a manifest rewrite = %v, want the pinned relay's edit to refuse", err)
	}
}

func hex32() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
