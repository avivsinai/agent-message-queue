package linkio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
)

// A signed grant is accepted at its issued_at. The same bytes 61 s later are
// grant_stale, unless they are the stored grant: presenting the stored grant
// again answers it unchanged (contract section 2, signed_grant).
func TestSignedGrantIsFreshUnlessStored(t *testing.T) {
	view, key := goldenView(t)
	var b Binding
	for _, v := range view.Bindings {
		b = v
	}
	issued := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	const gen = "g_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	doc, err := jcs.Marshal(GrantDocument{
		Schema: SchemaGrant, ServerID: view.ServerID, StoreID: view.StoreID,
		GrantID: "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Binding: b.Binding, TargetID: b.TargetID,
		Epoch: b.Epoch, NativeSessionID: b.NativeSessionID, Labels: b.Labels, GrantGeneration: gen,
		IssuedAt: issued.Format(time.RFC3339), NotAfter: issued.Add(time.Hour).Format(time.RFC3339),
		Input: GrantInput{Busy: "reject", Deliver: "turn", MinEvidence: "submitted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := SignedGrant(signDoc(t, key, doc))
	m.Schema = SchemaSignedGrant
	st := GrantState{Generation: gen, Available: true}

	got, err := VerifySignedGrant(view, m, st, issued)
	if err != nil {
		t.Fatalf("fresh grant refused: %v", err)
	}
	sum := sha256.Sum256(doc)
	if got.Digest != "sha256:"+hex.EncodeToString(sum[:]) || got.Generation != gen || got.NativeSessionID != b.NativeSessionID || got.CredentialID != key.CredentialID {
		t.Fatalf("stored %+v, want the signed grant", got)
	}

	later := issued.Add(61 * time.Second)
	var r *Refusal
	if _, err := VerifySignedGrant(view, m, st, later); !errors.As(err, &r) || r.Code != CodeGrantStale {
		t.Fatalf("61 s later: got %v, want %s", err, CodeGrantStale)
	}
	st.Stored = &got
	again, err := VerifySignedGrant(view, m, st, later)
	if err != nil || again != got {
		t.Fatalf("stored grant presented again: got %+v, %v; want the stored grant", again, err)
	}
}

// A generation is never seen twice across a detach and an attach of the
// same binding, and every mint clears the stored grant, so a grant signed
// before cannot be stored or revived again (contract attack: a counter
// restarted at detach revived old grants).
func TestMintGenerationEndsTheGrantForGood(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(LinkDir(stateDir, "l"), 0o700); err != nil {
		t.Fatal(err)
	}
	g1, err := EnsureGeneration(stateDir, "l", "b")
	if err != nil {
		t.Fatal(err)
	}
	old := StoredGrant{GrantID: "6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7", Binding: "b", Generation: g1}
	if err := StoreGrant(stateDir, "l", old); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{g1: true}
	for _, event := range []string{"detach", "attach"} {
		if err := MintGeneration(stateDir, "l", "b"); err != nil {
			t.Fatal(err)
		}
		g, err := LoadGrants(stateDir, "l")
		if err != nil {
			t.Fatal(err)
		}
		rec := g["b"]
		if seen[rec.Generation] || rec.Generation == "" || rec.Grant != nil {
			t.Fatalf("after %s: record %+v, want a new generation and no grant (seen %v)", event, rec, seen)
		}
		seen[rec.Generation] = true
	}
	if err := StoreGrant(stateDir, "l", old); !errors.Is(err, ErrGrantGeneration) {
		t.Fatalf("the old grant stored again: %v", err)
	}
}

// Contract section 1: a grant replaces the stored one only when it was
// issued later, so a replaced grant presented again inside its 60 s window
// stays retired (review of the session grant branch: an older grant replaced
// a newer, shorter one).
func TestStoreGrantKeepsTheNewerGrant(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(LinkDir(stateDir, "l"), 0o700); err != nil {
		t.Fatal(err)
	}
	gen, err := EnsureGeneration(stateDir, "l", "b")
	if err != nil {
		t.Fatal(err)
	}
	a := StoredGrant{Digest: "sha256:aa", Binding: "b", Generation: gen, IssuedAt: "2026-11-22T10:00:00Z", NotAfter: "2026-11-23T10:00:00Z"}
	b := StoredGrant{Digest: "sha256:bb", Binding: "b", Generation: gen, IssuedAt: "2026-11-22T10:00:20Z", NotAfter: "2026-11-22T11:00:20Z"}
	for _, sg := range []StoredGrant{a, b} {
		if err := StoreGrant(stateDir, "l", sg); err != nil {
			t.Fatalf("store %s: %v", sg.Digest, err)
		}
	}
	if err := StoreGrant(stateDir, "l", a); !errors.Is(err, ErrGrantNotNewer) {
		t.Fatalf("the older grant stored over the newer one: %v", err)
	}
	g, err := LoadGrants(stateDir, "l")
	if err != nil || g["b"].Grant == nil || g["b"].Grant.Digest != b.Digest {
		t.Fatalf("stored %+v, %v; want the newer grant %s", g["b"].Grant, err, b.Digest)
	}
}

// A server-revoked consent key is dropped even when the mint that ends the
// link's grants fails (review of the session grant branch: the key stayed on
// disk and was live again after a restart).
func TestDropConsentKeyStandsWhenTheMintFails(t *testing.T) {
	stateDir := t.TempDir()
	dir := LinkDir(stateDir, "l")
	// grants.json is a directory, so no new generation can be written.
	if err := os.MkdirAll(filepath.Join(dir, grantsFile, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteConsentKeys(stateDir, "l", []ConsentKey{{CredentialID: "k1"}}); err != nil {
		t.Fatal(err)
	}
	dropped, err := DropConsentKey(stateDir, "l", "k1")
	keys, rerr := LoadConsentKeys(stateDir, "l")
	if rerr != nil || len(keys) != 0 || !dropped {
		t.Fatalf("after the drop (dropped %v, err %v): keys %+v, %v; want none", dropped, err, keys, rerr)
	}
	if err == nil {
		t.Fatal("the failed mint was not reported")
	}
}
