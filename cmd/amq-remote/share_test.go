package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

func runShare(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := share(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("share %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), code
}

// ownerSign produces the owner-signed tag file for the given conditions.
func ownerSign(t *testing.T, dir, conditions string) string {
	t.Helper()
	owner := [32]byte{1}
	tag, err := bodykey.SignAuthTag(owner, "", conditions)
	if err != nil {
		t.Fatal(err)
	}
	return tag.SigHex()
}

// TestShareMintPreimageEnroll walks the acceptance path: share mints a 0600
// body key, prints the NIP-OA preimage; enrolling the owner-signed tag via
// --tag-file persists share.json and consumes the pending window.
func TestShareMintPreimageEnroll(t *testing.T) {
	root := t.TempDir()
	out, _, code := runShare(t, "--root", root, "--session", "s1")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var pre, conds, pub string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "preimage:"):
			pre = strings.TrimSpace(strings.TrimPrefix(line, "preimage:"))
		case strings.HasPrefix(line, "conditions:"):
			conds = strings.TrimSpace(strings.TrimPrefix(line, "conditions:"))
		case strings.HasPrefix(line, "body-pubkey:"):
			pub = strings.TrimSpace(strings.TrimPrefix(line, "body-pubkey:"))
		}
	}
	if pre == "" || conds == "" || pub == "" {
		t.Fatalf("share output missing fields: %q", out)
	}
	// The preimage is exactly the digest over the canonical NIP-OA string.
	keyDir := shareKeyDir(root, "s1")
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	if k.PublicKeyHex() != pub {
		t.Fatalf("printed pubkey %s != loaded %s", pub, k.PublicKeyHex())
	}
	if k.PreimageHex(conds) != pre {
		t.Fatalf("printed preimage does not match recomputation")
	}
	if fi, err := os.Stat(filepath.Join(keyDir, "body.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("body.key perm=%v err=%v, want 0600", fi.Mode().Perm(), err)
	}

	// Owner signs the printed preimage; enrollment via --tag-file.
	sig := ownerSignFor(t, k.PublicKeyHex(), conds)
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	if err := os.WriteFile(tagPath, []byte(`{"owner_pubkey":"`+ownerPubHex+`","conditions":"`+conds+`","sig":"`+sig+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out2, _, code := runShare(t, "--root", root, "--session", "s1", "--tag-file", tagPath)
	if code != 0 {
		t.Fatalf("enroll exit = %d: %s", code, out2)
	}
	if _, err := os.Stat(filepath.Join(keyDir, "share.json")); err != nil {
		t.Fatalf("enrolled share.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending window not consumed: %v", err)
	}
}

// ownerPubHex is the x-only pubkey of owner secret 0x...01 (the NIP-OA
// vector's owner).
const ownerPubHex = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

func ownerSignFor(t *testing.T, bodyPub, conditions string) string {
	t.Helper()
	var owner [32]byte
	owner[len(owner)-1] = 1
	tag, err := bodykey.SignAuthTag(owner, bodyPub, conditions)
	if err != nil {
		t.Fatal(err)
	}
	return tag.SigHex()
}

// TestShareRenewAfterExpiry covers `share --renew`: a new window prints a
// new preimage and the pending window updates; doctor warns within 7 days.
func TestShareRenewAfterExpiry(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "s2")
	// Renew prints a fresh pending window without touching the body key.
	keyDir := shareKeyDir(root, "s2")
	out, _, code := runShare(t, "--root", root, "--session", "s2", "--renew")
	if code != 0 {
		t.Fatalf("renew exit = %d", code)
	}
	var pending struct {
		Conditions string `json:"conditions"`
		NotAfter   int64  `json:"not_after"`
	}
	raw, err := os.ReadFile(filepath.Join(keyDir, "share.pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &pending); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, pending.Conditions) {
		t.Fatalf("renewed output does not carry the pending conditions")
	}

	// Doctor reports the session and warns when inside the 7-day horizon.
	// Backdate not_after by rewriting the pending doc.
	doc := map[string]any{}
	_ = json.Unmarshal(raw, &doc)
	doc["not_after"] = pending.NotAfter - 30*24*3600 + 5*24*3600 // 5 days out
	rewound, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(keyDir, "share.pending.json"), rewound, 0o600); err != nil {
		t.Fatal(err)
	}
	inspect := doctorShareInspection(root)
	if inspect == nil {
		t.Fatal("doctorShareInspection returned nil")
	}
	info, ok := inspect["s2"].(map[string]any)
	if !ok {
		t.Fatalf("session s2 missing from doctor inspection: %v", inspect)
	}
	warn, _ := info["warning"].(string)
	if !strings.Contains(warn, "--renew") {
		t.Fatalf("doctor warning missing renew hint: %q", warn)
	}
}

// TestShareRejectsWrongOwnerTag: a tag signed by a key that is not the
// claimed owner (self-attestation) is refused at enrollment.
func TestShareRejectsWrongOwnerTag(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "s3")
	keyDir := shareKeyDir(root, "s3")
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	conds := bodykey.ShareConditions(1893456000)
	sig := ownerSignFor(t, k.PublicKeyHex(), conds)
	// Self-attestation: owner pubkey == body pubkey.
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	if err := os.WriteFile(tagPath, []byte(`{"owner_pubkey":"`+k.PublicKeyHex()+`","conditions":"`+conds+`","sig":"`+sig+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code, err := share([]string{"--root", root, "--session", "s3", "--tag-file", tagPath}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatal("self-attested tag enrolled; want refusal")
	}
}
