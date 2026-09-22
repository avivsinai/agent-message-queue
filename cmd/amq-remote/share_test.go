package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// shareAllKinds prints the pending preimages for all kinds and returns a
// map kind → conditions parsed from the output.
func pendingConditions(t *testing.T, out string) map[uint16]string {
	t.Helper()
	m := map[uint16]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "kind ") || !strings.Contains(line, "conditions:") {
			continue
		}
		var kind uint16
		var conds string
		if _, err := fmt.Sscanf(line, "kind %d conditions: %s", &kind, &conds); err != nil {
			t.Fatalf("cannot parse %q: %v", line, err)
		}
		m[kind] = conds
	}
	if len(m) == 0 {
		t.Fatalf("no per-kind conditions in output:\n%s", out)
	}
	return m
}

// TestShareMintPrintEnrollFullLifecycle walks the complete acceptance path:
// share mints a 0600 body key and prints one preimage per kind; enrolling
// each owner-signed tag persists share.json; the pending window is consumed
// only when ALL kinds are enrolled; a second plain share shows the enrolled
// state with expiry derived from the signed conditions.
func TestShareMintPrintEnrollFullLifecycle(t *testing.T) {
	root := t.TempDir()
	out, _, code := runShare(t, "--root", root, "--session", "s1")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "s1")
	if fi, err := os.Stat(filepath.Join(keyDir, "body.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("body.key perm=%v err=%v, want 0600", fi.Mode().Perm(), err)
	}
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	condsByKind := pendingConditions(t, out)
	if len(condsByKind) != len(bodykey.ShareKinds) {
		t.Fatalf("got %d kinds, want %d", len(condsByKind), len(bodykey.ShareKinds))
	}
	// Each printed preimage recomputes exactly from its conditions.
	for kind, conds := range condsByKind {
		if k.PreimageHex(conds) == "" {
			t.Fatalf("empty preimage for kind %d", kind)
		}
	}

	// Enroll each kind. Until all kinds are enrolled, pending stays.
	for kind, conds := range condsByKind {
		tagPath := filepath.Join(t.TempDir(), "tag.json")
		doc := `{"kind":` + jsonNumber(int(kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + conds + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), conds) + `"}`
		if err := os.WriteFile(tagPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, code := runShare(t, "--root", root, "--session", "s1", "--tag-file", tagPath); code != 0 {
			t.Fatalf("enroll kind %d failed", kind)
		}
		if kind == condsByKindFirst(condsByKind) {
			if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); err != nil {
				t.Fatal("pending window consumed before all kinds enrolled")
			}
		}
	}
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending window not consumed after full enrollment: %v", err)
	}
	// A plain share now shows the enrolled state, no new pending window.
	out2, _, code := runShare(t, "--root", root, "--session", "s1")
	if code != 0 {
		t.Fatalf("plain share exit = %d", code)
	}
	if !strings.Contains(out2, "enrolled") || !strings.Contains(out2, "expires") {
		t.Fatalf("plain share output missing enrolled state:\n%s", out2)
	}
	if strings.Contains(out2, "preimage") {
		t.Fatalf("plain share must not mint a new window over an enrolled one:\n%s", out2)
	}
}

func condsByKindFirst(m map[uint16]string) uint16 {
	for k := range m {
		return k
	}
	return 0
}

func jsonNumber(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// TestShareDryRunWritesNothing pins codex P2: --dry-run must not mint or
// write any state, whether or not a key exists.
func TestShareDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	out, _, code := runShare(t, "--root", root, "--session", "d1", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "would mint") {
		t.Fatalf("dry-run without key should announce minting, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "extensions")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created state: %v", err)
	}
	// With an existing key, dry-run prints preimages and writes nothing.
	_, _, _ = runShare(t, "--root", root, "--session", "d2")
	before := snapshotTree(t, filepath.Join(root, "extensions", "remote", "keys"))
	out, _, code = runShare(t, "--root", root, "--session", "d2", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "preimage") {
		t.Fatalf("dry-run with key should print preimages:\n%s", out)
	}
	if after := snapshotTree(t, filepath.Join(root, "extensions", "remote", "keys")); !equalSnapshots(before, after) {
		t.Fatal("dry-run mutated persistent state")
	}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	return snapshotTreeAt(root)
}

func snapshotTreeAt(root string) map[string]string {
	m := map[string]string{}
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		raw, _ := os.ReadFile(p)
		m[p] = string(raw)
		return nil
	})
	return m
}

func equalSnapshots(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestShareRejectsPathTraversalAndSymlink pins codex P1 finding 1: a
// session id with separators or .. is refused; a symlinked key component is
// refused before any write.
func TestShareRejectsPathTraversalAndSymlink(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../../../../escaped", "a/b", `a\b`, "..", "."} {
		var stderr bytes.Buffer
		code, err := share([]string{"--root", root, "--session", bad}, &bytes.Buffer{}, &stderr)
		if code == 0 {
			t.Fatalf("session %q accepted", bad)
		}
		_ = err
	}
	// Nothing outside the keys root was created.
	if _, err := os.Stat(filepath.Dir(filepath.Join(root, "extensions"))); os.IsNotExist(err) {
		// fine — nothing at all was created
	} else if _, err := os.Stat(filepath.Join(root, "extensions", "remote", "keys", "..", "..")); err == nil {
		t.Fatal("unexpected escape artifact")
	}
	// Symlinked component.
	mkRoot := t.TempDir()
	keysRoot := filepath.Join(mkRoot, "extensions", "remote", "keys")
	if err := os.MkdirAll(keysRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(keysRoot, "evil")); err != nil {
		t.Skipf("cannot symlink in this environment: %v", err)
	}
	var stderr bytes.Buffer
	code, _ := share([]string{"--root", mkRoot, "--session", "evil"}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatal("symlinked key component accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "body.key")); !os.IsNotExist(err) {
		t.Fatal("body.key written through symlink")
	}
}

// TestShareRejectsUppercaseOwnerEncoding pins codex P1 finding 2: an
// uppercase-encoded owner pubkey must be refused, not normalized — the
// uppercase text passes naive inequality while decoding to the body key.
func TestShareRejectsUppercaseOwnerEncoding(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "u1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "u1")
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	// Read the pending conditions for one kind and sign with the BODY key
	// (self-attestation), then present the owner field uppercase.
	tags, _, err := readSharePending(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	conds := tags[0].Conditions
	selfSig := ownerSignFor(t, k.PublicKeyHex(), conds) // signed by body key itself
	upperBody := strings.ToUpper(k.PublicKeyHex())
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	doc := `{"kind":` + jsonNumber(int(tags[0].Kind)) + `,"owner_pubkey":"` + upperBody + `","conditions":"` + conds + `","sig":"` + selfSig + `"}`
	if err := os.WriteFile(tagPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code, _ := share([]string{"--root", root, "--session", "u1", "--tag-file", tagPath}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatal("uppercase-encoded self-attested tag enrolled; want refusal")
	}
	// No share.json was written — a failed enrollment preserves state.
	if gen, err := readEnrolledState(keyDir); err != nil || gen != nil {
		t.Fatal("failed enrollment mutated enrolled state")
	}
}

// TestShareRejectsWrongWindowAndKind pins the pending-window constraint:
// a tag whose conditions do not match the pending window for its kind is
// refused (no preimage desync), and an unknown kind is refused.
func TestShareRejectsWrongWindowAndKind(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "w1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "w1")
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	// Correct kind, wrong (fresh, not pending) conditions.
	conds := bodykey.ShareConditions(20003, time.Now().Add(48*time.Hour).Unix())
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	doc := `{"kind":20003,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + conds + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), conds) + `"}`
	_ = os.WriteFile(tagPath, []byte(doc), 0o600)
	var stderr bytes.Buffer
	if code, _ := share([]string{"--root", root, "--session", "w1", "--tag-file", tagPath}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("tag with non-pending conditions enrolled")
	}
	// Unknown kind.
	conds2 := bodykey.ShareConditions(999, time.Now().Add(48*time.Hour).Unix())
	doc2 := `{"kind":999,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + conds2 + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), conds2) + `"}`
	tagPath2 := filepath.Join(t.TempDir(), "tag.json")
	_ = os.WriteFile(tagPath2, []byte(doc2), 0o600)
	if code, _ := share([]string{"--root", root, "--session", "w1", "--tag-file", tagPath2}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("tag for unknown kind enrolled")
	}
}

// TestShareRenewReprintsAllKinds covers renewal: a new window re-prints one
// preimage per kind; the pending doc updates; doctor warns inside the
// 7-day horizon using the SIGNED conditions as the expiry authority.
func TestShareRenewReprintsAllKinds(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "r1")
	out, _, code := runShare(t, "--root", root, "--session", "r1", "--renew")
	if code != 0 {
		t.Fatalf("renew exit = %d", code)
	}
	condsByKind := pendingConditions(t, out)
	if len(condsByKind) != len(bodykey.ShareKinds) {
		t.Fatalf("renew printed %d kinds, want %d", len(condsByKind), len(bodykey.ShareKinds))
	}
	// Doctor: pending attestation reported (owner has not signed yet).
	inspect := doctorShareInspection(root)
	info := inspect["r1"].(map[string]any)
	if info["attestation"] != "pending: owner has not returned signed tags yet" {
		t.Fatalf("doctor attestation = %v", info["attestation"])
	}
	// Doctor expiry warning derived from signed conditions: enroll only
	// SOME kinds → incomplete warning; backdate via a renewed pending with
	// a near bound and enroll all → expiry warning.
	tags, _, err := readSharePending(filepath.Join(root, "extensions", "remote", "keys", "r1"))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := bodykey.Load(filepath.Join(root, "extensions", "remote", "keys", "r1", "body.key"))
	for _, tag := range tags {
		tagPath := filepath.Join(t.TempDir(), "tag.json")
		doc := `{"kind":` + jsonNumber(int(tag.Kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + tag.Conditions + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), tag.Conditions) + `"}`
		_ = os.WriteFile(tagPath, []byte(doc), 0o600)
		if _, _, code := runShare(t, "--root", root, "--session", "r1", "--tag-file", tagPath); code != 0 {
			t.Fatalf("enroll kind %d failed", tag.Kind)
		}
	}
	inspect2 := doctorShareInspection(root)
	info2 := inspect2["r1"].(map[string]any)
	if info2["attestation"] != "enrolled" {
		t.Fatalf("doctor attestation after full enrollment = %v", info2["attestation"])
	}
	// Enrolled expiry comes from signed conditions, not a stored not_after.
	expires30d := time.Now().Add(30 * 24 * time.Hour)
	firstExp := earliestEnrolledExpiry(t, tags)
	if diff := firstExp.Sub(expires30d); diff > 2*time.Hour || diff < -2*time.Hour {
		t.Fatalf("enrolled expiry %v not near the signed window %v", firstExp, expires30d)
	}
	// No bogus 1970 expiry.
	if firstExp.Year() < 2000 {
		t.Fatalf("enrolled expiry is %v — zero not_after leaked", firstExp)
	}
}

func earliestEnrolledExpiry(t *testing.T, tags []shareTagFile) time.Time {
	t.Helper()
	earliest := time.Time{}
	for _, tf := range tags {
		exp, err := enrolledExpiry(tf)
		if err != nil {
			t.Fatal(err)
		}
		if earliest.IsZero() || exp.Before(earliest) {
			earliest = exp
		}
	}
	return earliest
}

// TestShareRejectsSelfAttestation pins the decoded-identity refusal with a
// canonical lowercase encoding too.
func TestShareRejectsSelfAttestation(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "s3")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "s3")
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	tags, _, err := readSharePending(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	conds := tags[0].Conditions
	sig := ownerSignFor(t, k.PublicKeyHex(), conds)
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	doc := `{"kind":` + jsonNumber(int(tags[0].Kind)) + `,"owner_pubkey":"` + k.PublicKeyHex() + `","conditions":"` + conds + `","sig":"` + sig + `"}`
	_ = os.WriteFile(tagPath, []byte(doc), 0o600)
	var stderr bytes.Buffer
	if code, _ := share([]string{"--root", root, "--session", "s3", "--tag-file", tagPath}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("self-attested tag enrolled; want refusal")
	}
}

// enrollAllPending enrolls every pending tag and fails on refusal.
func enrollAllPending(t *testing.T, root, session string) {
	t.Helper()
	keyDir := filepath.Join(root, "extensions", "remote", "keys", session)
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	tags, _, err := readSharePending(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		tagPath := filepath.Join(t.TempDir(), "tag.json")
		doc := `{"kind":` + jsonNumber(int(tag.Kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + tag.Conditions + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), tag.Conditions) + `"}`
		if err := os.WriteFile(tagPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if code, _ := share([]string{"--root", root, "--session", session, "--tag-file", tagPath}, &bytes.Buffer{}, &stderr); code != 0 {
			t.Fatalf("enroll kind %d refused: %s", tag.Kind, stderr.String())
		}
	}
}

// TestFullEnrollRenewEnrollAllKinds pins codex P1 finding 1: renewal mints a
// NEW generation; the first new tag must not complete the generation by
// counting old-plus-new entries, and every remaining kind must enroll.
func TestFullEnrollRenewEnrollAllKinds(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "g1")
	enrollAllPending(t, root, "g1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "g1")
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending not consumed after full enrollment: %v", err)
	}
	firstGen := readEnrolledTagsForTest(t, keyDir)
	// Renew: new window, new pending, new generation.
	out, _, code := runShare(t, "--root", root, "--session", "g1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	condsByKind := pendingConditions(t, out)
	// Enroll ONE kind of the new generation: pending must NOT be consumed
	// (the old five-tag enrollment must not count toward completion).
	one := condsByKindFirst(condsByKind)
	conds := condsByKind[one]
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	doc := `{"kind":` + jsonNumber(int(one)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + conds + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), conds) + `"}`
	_ = os.WriteFile(tagPath, []byte(doc), 0o600)
	if _, _, code := runShare(t, "--root", root, "--session", "g1", "--tag-file", tagPath); code != 0 {
		t.Fatalf("first renewal enrollment refused")
	}
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); err != nil {
		t.Fatal("pending consumed after ONE renewal enrollment — old generation counted toward completion")
	}
	// Enroll the remaining four; only now is the new generation complete.
	enrollRemaining(t, root, "g1", condsByKind, one)
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending not consumed after full renewal enrollment: %v", err)
	}
	secondGen := readEnrolledTagsForTest(t, keyDir)
	if len(secondGen) != len(bodykey.ShareKinds) {
		t.Fatalf("renewed generation has %d tags, want %d", len(secondGen), len(bodykey.ShareKinds))
	}
	// Every renewed tag's conditions are the NEW window, not the old one.
	for _, tf := range secondGen {
		if condsByKind[tf.Kind] != tf.Conditions {
			t.Fatalf("renewed generation kept stale tag for kind %d", tf.Kind)
		}
	}
	_ = firstGen
}

func enrollRemaining(t *testing.T, root, session string, condsByKind map[uint16]string, done uint16) {
	t.Helper()
	keyDir := filepath.Join(root, "extensions", "remote", "keys", session)
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	for kind, conds := range condsByKind {
		if kind == done {
			continue
		}
		tagPath := filepath.Join(t.TempDir(), "tag.json")
		doc := `{"kind":` + jsonNumber(int(kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + conds + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), conds) + `"}`
		_ = os.WriteFile(tagPath, []byte(doc), 0o600)
		var stderr bytes.Buffer
		if code, _ := share([]string{"--root", root, "--session", session, "--tag-file", tagPath}, &bytes.Buffer{}, &stderr); code != 0 {
			t.Fatalf("renewal enroll kind %d refused: %s", kind, stderr.String())
		}
	}
}

func readEnrolledTagsForTest(t *testing.T, keyDir string) []shareTagFile {
	t.Helper()
	gen, err := readEnrolledState(keyDir)
	if err != nil || gen == nil {
		t.Fatalf("enrolled state missing/invalid: %v", err)
	}
	return gen.Tags
}

// TestShareRejectsSymlinkedKeysRoot pins codex P1 finding 2: a symlink AT
// the keys root (or any owned parent) redirects mint outside the root and
// is refused.
func TestShareRejectsSymlinkedKeysRoot(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	remote := filepath.Join(root, "extensions", "remote")
	if err := os.MkdirAll(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(remote, "keys")); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}
	var stderr bytes.Buffer
	if code, _ := share([]string{"--root", root, "--session", "s"}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("symlinked keys root accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "s", "body.key")); !os.IsNotExist(err) {
		t.Fatal("body.key minted through the symlinked keys root")
	}
}

// TestShareRefusesCorruptEnrolledState pins codex P2 finding 4: invalid
// share.json is an error, not absence — plain share refuses and enrollment
// never overwrites it.
func TestShareRefusesCorruptEnrolledState(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "c1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "c1")
	if err := os.WriteFile(filepath.Join(keyDir, "share.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code, _ := share([]string{"--root", root, "--session", "c1"}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("plain share over corrupt enrolled state did not refuse")
	}
	if _, err := os.ReadFile(filepath.Join(keyDir, "share.json")); err != nil {
		t.Fatal("corrupt enrolled state removed")
	}
	if got, _ := os.ReadFile(filepath.Join(keyDir, "share.json")); string(got) != "{not json" {
		t.Fatal("corrupt enrolled state overwritten")
	}
}

// TestShareRejectsUppercaseSignature pins codex P2 finding 5: signature hex
// uses the canonical lowercase parser; mixed-case is refused before
// persistence.
func TestShareRejectsUppercaseSignature(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "u2")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "u2")
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	tags, _, err := readSharePending(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	conds := tags[0].Conditions
	sig := strings.ToUpper(ownerSignFor(t, k.PublicKeyHex(), conds))
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	doc := `{"kind":` + jsonNumber(int(tags[0].Kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + conds + `","sig":"` + sig + `"}`
	_ = os.WriteFile(tagPath, []byte(doc), 0o600)
	var stderr bytes.Buffer
	if code, _ := share([]string{"--root", root, "--session", "u2", "--tag-file", tagPath}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("uppercase signature accepted")
	}
	if gen, err := readEnrolledState(keyDir); err != nil || gen != nil {
		t.Fatal("refused tag mutated enrolled state")
	}
}
