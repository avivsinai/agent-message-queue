package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/sharestate"
)

// 611.16: `share --enable buzz-dm` asks the owner for kind 9 and 40003
// grants on top of the base kinds; once every kind is signed the enrolled
// generation carries them and the runtime loader serves a kind 9 grant.
func TestShareEnableBuzzDMEnrollsDMKinds(t *testing.T) {
	root := t.TempDir()
	out, _, _ := runShare(t, "--root", root, "--session", "dm", "--enable", "buzz-dm")
	conds := pendingConditions(t, out)
	if len(conds) != len(bodykey.ShareKinds)+2 || conds[9] == "" || conds[40003] == "" {
		t.Fatalf("window kinds = %v, want the base kinds plus 9 and 40003", conds)
	}
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "dm")
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	for kind, c := range conds {
		tagPath := filepath.Join(t.TempDir(), "tag.json")
		doc := `{"kind":` + jsonNumber(int(kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + c + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), c) + `"}`
		if err := os.WriteFile(tagPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		runShare(t, "--root", root, "--session", "dm", "--tag-file", tagPath)
	}
	creds, err := sharestate.Load(root, "dm")
	if err != nil {
		t.Fatalf("load enrolled credentials: %v", err)
	}
	if _, err := creds.TagFor(9, time.Now()); err != nil {
		t.Fatalf("kind 9 grant: %v", err)
	}
	if _, err := creds.TagFor(40003, time.Now()); err != nil {
		t.Fatalf("kind 40003 grant: %v", err)
	}
}
