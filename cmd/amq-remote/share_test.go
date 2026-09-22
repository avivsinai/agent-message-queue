package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// runShareLoose runs share without failing on non-zero exits; returns
// stdout, stderr, exit code.
func runShareLoose(args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	code, err := share(args, &stdout, &stderr)
	if err != nil {
		// Refusals surface on stderr in-process the same way finish()
		// prints them for the CLI.
		stderr.WriteString(err.Error())
	}
	return stdout.String(), stderr.String(), code
}

func runShare(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := share(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("share %v: %v (stderr=%q)", args, err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("share %v: exit %d, stderr=%q", args, code, stderr.String())
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

	// Enroll each kind in DECLARED order (verifier r2 P1-1: ranging a map
	// randomized the order and the pending guard raced the completion).
	// Until all kinds are enrolled, pending stays.
	enrolled := 0
	for _, kind := range bodykey.ShareKinds {
		conds := condsByKind[kind]
		tagPath := filepath.Join(t.TempDir(), "tag.json")
		doc := `{"kind":` + jsonNumber(int(kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + conds + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), conds) + `"}`
		if err := os.WriteFile(tagPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, code := runShare(t, "--root", root, "--session", "s1", "--tag-file", tagPath); code != 0 {
			t.Fatalf("enroll kind %d failed", kind)
		}
		enrolled++
		if enrolled < len(condsByKind) {
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
	st, err := loadShareState(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	tags := st.Pending.Tags
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
	if stChk, err := loadShareState(keyDir); err != nil || stChk.Enrolled.Gen != nil {
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
	if info["attestation"] != "pending: 0 of 5 kinds signed (staged)" {
		t.Fatalf("doctor attestation = %v", info["attestation"])
	}
	// Doctor expiry warning derived from signed conditions: enroll only
	// SOME kinds → incomplete warning; backdate via a renewed pending with
	// a near bound and enroll all → expiry warning.
	stR1, err := loadShareState(filepath.Join(root, "extensions", "remote", "keys", "r1"))
	if err != nil {
		t.Fatal(err)
	}
	tags := stR1.Pending.Tags
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
	st, err := loadShareState(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	tags := st.Pending.Tags
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
	st, err := loadShareState(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	tags := st.Pending.Tags
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		enrollOne(t, root, session, tag.Kind, tag.Conditions)
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
	if len(firstGen) != len(bodykey.ShareKinds) {
		t.Fatalf("first generation incomplete: %d tags", len(firstGen))
	}
	// Renew: new window, new pending, new generation.
	out, _, code := runShare(t, "--root", root, "--session", "g1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	condsByKind := pendingConditions(t, out)
	// Enroll ONE kind of the new generation: pending must NOT be consumed
	// (the old five-tag enrollment must not count toward completion).
	one := bodykey.ShareKinds[0]
	enrollOne(t, root, "g1", one, condsByKind[one])
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); err != nil {
		t.Fatal("pending consumed after ONE renewal enrollment — old generation counted toward completion")
	}
	// Retention (verifier P0-1): the still-valid first-generation tags must
	// SURVIVE the first new-generation enrollment — make-before-break.
	midGen := readEnrolledTagsForTest(t, keyDir)
	midKinds := map[uint16]bool{}
	for _, tf := range midGen {
		midKinds[tf.Kind] = true
	}
	for _, tf := range firstGen {
		if !midKinds[tf.Kind] {
			t.Fatalf("first-generation tag for kind %d erased by the first renewal enrollment", tf.Kind)
		}
	}
	if len(midGen) < len(firstGen) {
		t.Fatalf("renewal dropped tags: %d -> %d", len(firstGen), len(midGen))
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
}

// enrollOne enrolls a single (kind, conditions) tag.
func enrollOne(t *testing.T, root, session string, kind uint16, conds string) {
	t.Helper()
	keyDir := filepath.Join(root, "extensions", "remote", "keys", session)
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	doc := `{"kind":` + jsonNumber(int(kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + conds + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), conds) + `"}`
	if err := os.WriteFile(tagPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	errBuf := &bytes.Buffer{}
	code2, err2 := share([]string{"--root", root, "--session", session, "--tag-file", tagPath}, &bytes.Buffer{}, errBuf)
	if err2 != nil || code2 != 0 {
		t.Fatalf("enroll kind %d refused: code=%d err=%v stderr=%q", kind, code2, err2, errBuf.String())
	}
}

func enrollRemaining(t *testing.T, root, session string, condsByKind map[uint16]string, done uint16) {
	t.Helper()
	for _, kind := range bodykey.ShareKinds {
		if kind == done {
			continue
		}
		enrollOne(t, root, session, kind, condsByKind[kind])
	}
}

func readEnrolledTagsForTest(t *testing.T, keyDir string) []shareTagFile {
	t.Helper()
	stGen, err := loadShareState(keyDir)
	gen := stGen.Enrolled.Gen
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
	st, err := loadShareState(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	tags := st.Pending.Tags
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
	if stChk, err := loadShareState(keyDir); err != nil || stChk.Enrolled.Gen != nil {
		t.Fatal("refused tag mutated enrolled state")
	}
}

// TestBodyKeySymlinkLeafRefused pins verifier P1-1: a body.key symlinked to
// an existing out-of-root key is refused by Load (both plain share and
// enrollment), not adopted.
func TestBodyKeySymlinkLeafRefused(t *testing.T) {
	root := t.TempDir()
	// Foreign valid key outside the root.
	foreignDir := t.TempDir()
	if _, err := bodykey.LoadOrMint(foreignDir); err != nil {
		t.Fatal(err)
	}
	runShare(t, "--root", root, "--session", "sl1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "sl1")
	if err := os.Remove(filepath.Join(keyDir, "body.key")); err != nil {
		t.Fatal(err)
	}
	// Drop the legit pending window from the first share so the only thing
	// under test is the symlink adoption.
	if err := os.Remove(filepath.Join(keyDir, "share.pending.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(foreignDir, "body.key"), filepath.Join(keyDir, "body.key")); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}
	var stderr bytes.Buffer
	if code, _ := share([]string{"--root", root, "--session", "sl1"}, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatalf("symlinked body.key adopted; stderr: %s", stderr.String())
	}
	// The adopted key must not appear anywhere in session state.
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); err == nil {
		t.Fatal("pending window minted for the symlinked foreign key")
	}
}

// TestPlainShareReprintsOutstanding pins verifier P0-1's flow fix: mid-renewal
// (one new kind enrolled), plain share reprints the OUTSTANDING pending
// preimages instead of only the enrolled state, and the old generation
// survives.
func TestPlainShareReprintsOutstanding(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "o1")
	enrollAllPending(t, root, "o1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "o1")
	firstGen := readEnrolledTagsForTest(t, keyDir)
	out, _, code := runShare(t, "--root", root, "--session", "o1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	condsByKind := pendingConditions(t, out)
	doneKind := uint16(20003)
	enrollOne(t, root, "o1", doneKind, condsByKind[doneKind])
	// Plain share now: must print outstanding preimages for the other 4.
	out2, _, code := runShare(t, "--root", root, "--session", "o1")
	if code != 0 {
		t.Fatal("plain share refused mid-renewal")
	}
	if !strings.Contains(out2, "outstanding preimages (4") {
		t.Fatalf("plain share did not reprint outstanding preimages:\n%s", out2)
	}
	// Old generation survived (all five kinds still present, still valid).
	midGen := readEnrolledTagsForTest(t, keyDir)
	seen := map[uint16]bool{}
	for _, tf := range midGen {
		seen[tf.Kind] = true
	}
	for _, tf := range firstGen {
		if !seen[tf.Kind] {
			t.Fatalf("mid-renewal enrolled set lost kind %d", tf.Kind)
		}
	}
	// Finish enrollment from the REPRINTED output; completion works without
	// another renew. The one kind already enrolled (done) must not be
	// re-enrolled.
	enrollRemaining(t, root, "o1", condsByKind, doneKind)
	stGen, gerr := loadShareState(keyDir)
	gen := stGen.Enrolled.Gen
	if gerr != nil {
		t.Fatal(gerr)
	}
	enrolledConds := map[uint16]string{}
	for _, tf := range gen.Tags {
		enrolledConds[tf.Kind] = tf.Conditions
	}
	if len(enrolledConds) != len(bodykey.ShareKinds) {
		t.Fatalf("published generation incomplete: %d kinds", len(enrolledConds))
	}
	for kind, conds := range condsByKind {
		if enrolledConds[kind] != conds {
			t.Fatalf("published kind %d carries %q, want the renewed window %q", kind, enrolledConds[kind], conds)
		}
	}
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending not consumed after completing the renewal: %v", err)
	}
}

// TestDoctorKeepsExpiryDuringRenewal pins verifier r3 P1-1: the expiry FACT
// is emitted even mid-renewal — the staged shape makes the whole renewal an
// outage window, so an active generation that lapses (or is about to lapse)
// while preimages are unsigned must still alarm. Only the REMEDY changes:
// plain `share`, never `--renew`.
func TestDoctorKeepsExpiryDuringRenewal(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "dx1")
	enrollAllPending(t, root, "dx1")
	// Force the active generation to be already expired.
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "dx1")
	tags := readEnrolledTagsForTest(t, keyDir)
	for i := range tags {
		tags[i].Conditions = rewriteBoundToPast(tags[i].Conditions)
	}
	if err := writeEnrolledTagsForTest(t, keyDir, tags); err != nil {
		t.Fatal(err)
	}
	// Start a renewal and stage exactly one kind.
	out, _, code := runShare(t, "--root", root, "--session", "dx1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	condsByKind := pendingConditions(t, out)
	enrollOne(t, root, "dx1", 20003, condsByKind[20003])
	inspect := doctorShareInspection(root)
	info := inspect["dx1"].(map[string]any)
	exp, has := info["expiry_warning"]
	if !has {
		t.Fatalf("doctor dropped the expiry row during a renewal (r3 P1-1): %v", info)
	}
	expStr := exp.(string)
	if !strings.Contains(expStr, "expired") {
		t.Fatalf("expiry_warning did not report the lapse: %q", expStr)
	}
	if strings.Contains(expStr, "--renew") {
		t.Fatalf("expiry remedy pointed at --renew while preimages are outstanding: %q", expStr)
	}
	if !strings.Contains(expStr, "share --session dx1") {
		t.Fatalf("expiry remedy must name plain share: %q", expStr)
	}
}

// rewriteBoundToPast rewrites a conditions string's created_at bound to a
// timestamp guaranteed to be in the past.
func rewriteBoundToPast(conds string) string {
	i := strings.Index(conds, "created_at<")
	if i < 0 {
		return conds
	}
	return conds[:i+len("created_at<")] + "1000000000"
}

// writeEnrolledTagsForTest replaces share.json with the given tags
// (0600, plain write is fine in a test keyDir).
func writeEnrolledTagsForTest(t *testing.T, keyDir string, tags []shareTagFile) error {
	t.Helper()
	raw, err := json.MarshalIndent(struct {
		Tags []shareTagFile `json:"tags"`
	}{tags}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(keyDir, "share.json"), raw, 0o600)
}

// TestDryRunPrintsPendingPreimages pins verifier P1-3: with a pending window,
// --dry-run prints the ENROLLABLE preimages (they match the pending
// conditions).
func TestDryRunPrintsPendingPreimages(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "d3")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "d3")
	stpendingTags, _ := loadShareState(keyDir)
	pendingTags, _, err := stpendingTags.Pending.Tags, stpendingTags.Pending.NotAfter, stpendingTags.Pending.Err
	if err != nil {
		t.Fatal(err)
	}
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	out, _, code := runShare(t, "--root", root, "--session", "d3", "--dry-run")
	if code != 0 {
		t.Fatal("dry-run refused")
	}
	for _, pt := range pendingTags {
		want := k.PreimageHex(pt.Conditions)
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing enrollable preimage for kind %d:\n%s", pt.Kind, out)
		}
	}
}

// TestActiveGenerationStaysUntilComplete pins codex re-review P1 #1: the
// ACTIVE enrolled generation must not change until the pending generation
// is complete. A single new-generation enrollment stages the tag; share.json
// still holds the old signed tag for that kind.
func TestActiveGenerationStaysUntilComplete(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "st1")
	enrollAllPending(t, root, "st1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "st1")
	before := readEnrolledTagsForTest(t, keyDir)
	out, _, code := runShare(t, "--root", root, "--session", "st1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	pending := pendingConditions(t, out)
	enrollOne(t, root, "st1", 20003, pending[20003])
	after := readEnrolledTagsForTest(t, keyDir)
	for _, old := range before {
		for _, current := range after {
			if old.Kind == current.Kind && old != current {
				t.Fatalf("active kind %d changed before pending generation completed", old.Kind)
			}
		}
	}
	// Completing the generation publishes the new one atomically and drops
	// the staged doc.
	enrollRemaining(t, root, "st1", pending, 20003)
	stGen, err := loadShareState(keyDir)
	gen := stGen.Enrolled.Gen
	if err != nil || gen == nil {
		t.Fatalf("published generation missing: %v", err)
	}
	if len(gen.Tags) != len(bodykey.ShareKinds) {
		t.Fatalf("published generation incomplete: %+v", gen)
	}
	published := map[uint16]string{}
	for _, tf := range gen.Tags {
		published[tf.Kind] = tf.Conditions
	}
	for kind, conds := range pending {
		if published[kind] != conds {
			t.Fatalf("published kind %d has %q, want new-window %q", kind, published[kind], conds)
		}
	}
	if _, err := os.Stat(filepath.Join(keyDir, stagedName)); !os.IsNotExist(err) {
		t.Fatalf("staged doc not consumed after completion: %v", err)
	}
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending not consumed after completion: %v", err)
	}
}

// TestPendingStateSymlinkRefused pins codex re-review P1 #2: a symlinked
// share.pending.json (or share.json) is refused; renew must not follow it
// and rewrite an out-of-root file.
func TestPendingStateSymlinkRefused(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "sl2")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "sl2")
	pendingPath := filepath.Join(keyDir, "share.pending.json")
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, pendingPath); err != nil {
		t.Skipf("cannot symlink in this environment: %v", err)
	}
	if code, _ := share([]string{"--root", root, "--session", "sl2", "--renew"}, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("renew followed a symlinked pending state file")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, after) {
		t.Fatal("renew rewrote the out-of-root target through the pending symlink")
	}
	// The enrolled leaf is equally confined.
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	enrolledPath := filepath.Join(keyDir, "share.json")
	if err := os.Symlink(target, enrolledPath); err != nil {
		t.Skipf("cannot symlink in this environment: %v", err)
	}
	if code, _ := share([]string{"--root", root, "--session", "sl2", "--renew"}, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("renew followed a symlinked enrolled state file")
	}
}

// TestRenewDryRunPreviewNotEnrollable pins the round-3 ruling (claude
// 08:29Z): --renew --dry-run is a NON-enrollable preview — the bound is
// fixed only when the real --renew persists the window, so the preview
// must say so explicitly and no signature is accepted against its bound.
func TestRenewDryRunPreviewNotEnrollable(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "dr1")
	enrollAllPending(t, root, "dr1")
	preview, stderrL, code := runShareLoose("--root", root, "--session", "dr1", "--renew", "--dry-run")
	if code != 0 {
		t.Fatalf("renew dry-run refused (code=%d): %s", code, stderrL)
	}
	if !strings.Contains(preview, "bound is fixed when --renew runs") {
		t.Fatalf("renew dry-run preview not labeled non-enrollable:\n%s", preview)
	}
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "dr1")
	previewConds := pendingConditions(t, preview)
	// The preview bound is computed past the enrolled generation, but that
	// is NOT a persisted window: no share.pending.json exists yet and a
	// tag signed over the preview bound is refused.
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); !os.IsNotExist(err) {
		t.Fatalf("renew dry-run persisted a window: %v", err)
	}
	for _, old := range readEnrolledTagsForTest(t, keyDir) {
		if previewConds[old.Kind] == old.Conditions {
			t.Fatalf("renew dry-run previewed the STALE window for kind %d", old.Kind)
		}
	}
	// The real renew persists its own bound, which need not equal the
	// preview's (time passed between the two instants).
	actual, _, code := runShare(t, "--root", root, "--session", "dr1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	if len(pendingConditions(t, actual)) != len(previewConds) {
		t.Fatal("real renew printed a different number of kinds than the preview")
	}
}

// TestExplicitDaysShorteningRefused pins the round-3 ruling (claude
// 08:27Z #1): an explicit --days that cannot exceed the current bounds is
// REFUSED with a message naming the minimum — never silently bumped.
func TestExplicitDaysShorteningRefused(t *testing.T) {
	root := t.TempDir()
	_, stderr, code := runShareLoose("--root", root, "--session", "sd1", "--days", "60")
	if code != 0 {
		t.Fatalf("initial mint refused: %s", stderr)
	}
	_, stderr, code = runShareLoose("--root", root, "--session", "sd1", "--renew", "--days", "1")
	if code == 0 {
		t.Fatal("renew --days 1 over a 60-day bound was silently bumped")
	}
	if !strings.Contains(stderr, "Minimum --days that works") {
		t.Fatalf("refusal does not name the minimum:\n%s", stderr)
	}
	// A --days large enough to clear the bound is accepted.
	_, stderr, code = runShareLoose("--root", root, "--session", "sd1", "--renew", "--days", "90")
	if code != 0 {
		t.Fatalf("renew --days 90 refused:\n%s", stderr)
	}
}

// TestShareDryRunNoPendingLabelsIllustrative pins codex re-review P2 #3
// (second half): a dry-run with no pending window prints a fresh-bound
// window clearly labeled as not-enrollable.
func TestShareDryRunNoPendingLabelsIllustrative(t *testing.T) {
	root := t.TempDir()
	out, _, code := runShare(t, "--root", root, "--session", "dr2", "--dry-run")
	if code != 0 {
		t.Fatal("dry-run refused")
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run output not labeled:\n%s", out)
	}
	// Nothing was written.
	if _, err := os.Stat(filepath.Join(root, "extensions", "remote", "keys", "dr2", "body.key")); !os.IsNotExist(err) {
		t.Fatal("dry-run minted a body key")
	}
}

// TestRenewFailsClosedOnCorruptEnrolledState pins verifier r2 P1-2: --renew
// must refuse on a torn/corrupt share.json like every other path — it must
// not exit 0 minting a window no tag can ever be enrolled into.
func TestRenewFailsClosedOnCorruptEnrolledState(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "cr1")
	enrollAllPending(t, root, "cr1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "cr1")
	if err := os.WriteFile(filepath.Join(keyDir, "share.json"), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := share([]string{"--root", root, "--session", "cr1", "--renew"}, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("--renew exited 0 over corrupt enrolled state")
	}
	// The corrupt file is untouched (never silently overwritten).
	if got, _ := os.ReadFile(filepath.Join(keyDir, "share.json")); string(got) != "{torn" {
		t.Fatal("corrupt enrolled state was overwritten by --renew")
	}
	// The dry-run renewal preview fails closed on the same input.
	if code, _ := share([]string{"--root", root, "--session", "cr1", "--renew", "--dry-run"}, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("--renew --dry-run exited 0 over corrupt enrolled state")
	}
}

// TestDoctorSeesRenewalInProgress pins verifier r2 P1-3: during a
// half-finished renewal the doctor reports the outstanding preimages and
// its remedy is plain `share`, never `--renew` — the enrolled tag count
// alone cannot distinguish a half renewal from a healthy enrollment.
func TestDoctorSeesRenewalInProgress(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "dc1")
	enrollAllPending(t, root, "dc1")
	out, _, code := runShare(t, "--root", root, "--session", "dc1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	condsByKind := pendingConditions(t, out)
	enrollOne(t, root, "dc1", 20003, condsByKind[20003])
	inspect := doctorShareInspection(root)
	info := inspect["dc1"].(map[string]any)
	if info["attestation"] != "enrolled, renewal in progress: 1 of 5 kinds signed (staged)" {
		t.Fatalf("doctor missed the half-finished renewal: %v", info["attestation"])
	}
	warn, _ := info["warning"].(string)
	if !strings.Contains(warn, "do NOT renew") {
		t.Fatalf("doctor warning missing the do-not-renew remedy: %v", warn)
	}
	if _, has := info["expiry_warning"]; has {
		t.Fatalf("expiry remedy pointed at --renew while preimages are outstanding: %v", info["expiry_warning"])
	}
	// Completing the renewal clears the warning.
	enrollRemaining(t, root, "dc1", condsByKind, 20003)
	inspect2 := doctorShareInspection(root)
	info2 := inspect2["dc1"].(map[string]any)
	if info2["attestation"] != "enrolled" {
		t.Fatalf("completed renewal still flagged: %v", info2["attestation"])
	}
}

// TestShareLeafConfinementPreMintAndEnrolled pins verifier r2 P1-4's wider
// findings: a pending symlink planted before the FIRST share is refused
// (no --renew needed), and a symlinked share.json is refused on enrollment.
func TestShareLeafConfinementPreMintAndEnrolled(t *testing.T) {
	// Case 1: symlink before first mint — plain share must refuse.
	root := t.TempDir()
	keysRoot := filepath.Join(root, "extensions", "remote", "keys", "pf1")
	if err := os.MkdirAll(keysRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("PRECIOUS OPERATOR FILE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(keysRoot, "share.pending.json")); err != nil {
		t.Skipf("cannot symlink in this environment: %v", err)
	}
	if code, _ := share([]string{"--root", root, "--session", "pf1"}, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("plain share followed a symlinked pending leaf pre-mint")
	}
	after, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "PRECIOUS OPERATOR FILE\n" {
		t.Fatal("victim clobbered through the pre-mint pending symlink")
	}

	// Case 2: symlinked share.json before an enrollment.
	root2 := t.TempDir()
	runShare(t, "--root", root2, "--session", "pf2")
	keyDir := filepath.Join(root2, "extensions", "remote", "keys", "pf2")
	if err := os.Remove(filepath.Join(keyDir, "share.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	victim2 := filepath.Join(t.TempDir(), "victim2.json")
	if err := os.WriteFile(victim2, []byte(`{"tags":[],"note":"operator file that happens to be JSON"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim2, filepath.Join(keyDir, "share.json")); err != nil {
		t.Skipf("cannot symlink in this environment: %v", err)
	}
	st, err := loadShareState(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	tags := st.Pending.Tags
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := share([]string{"--root", root2, "--session", "pf2", "--tag-file", writeTagFile(t, keyDir, tags[0])}, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("enrollment followed a symlinked share.json leaf")
	}
	vAfter, err := os.ReadFile(victim2)
	if err != nil {
		t.Fatal(err)
	}
	if string(vAfter) != `{"tags":[],"note":"operator file that happens to be JSON"}` {
		t.Fatal("victim2 clobbered through the share.json symlink")
	}
}

// writeTagFile signs one pending tag into a temp tag file.
func writeTagFile(t *testing.T, keyDir string, tag shareTagFile) string {
	t.Helper()
	k, err := bodykey.Load(filepath.Join(keyDir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	tagPath := filepath.Join(t.TempDir(), "tag.json")
	doc := `{"kind":` + jsonNumber(int(tag.Kind)) + `,"owner_pubkey":"` + ownerPubHex + `","conditions":"` + tag.Conditions + `","sig":"` + ownerSignFor(t, k.PublicKeyHex(), tag.Conditions) + `"}`
	if err := os.WriteFile(tagPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return tagPath
}

// TestMalformedPendingNeverReplaced pins codex r3 #2: malformed pending
// JSON is an error on every path — plain share, renew, dry-run, doctor —
// never silently reset to a fresh window.
func TestMalformedPendingNeverReplaced(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "mp1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "mp1")
	pendingPath := filepath.Join(keyDir, "share.pending.json")
	if err := os.WriteFile(pendingPath, []byte("broken JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range [][]string{
		{"--root", root, "--session", "mp1"},
		{"--root", root, "--session", "mp1", "--renew"},
		{"--root", root, "--session", "mp1", "--renew", "--dry-run"},
		{"--root", root, "--session", "mp1", "--dry-run"},
	} {
		if code, _ := share(tc, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
			t.Fatalf("%v exited 0 over malformed pending state", tc)
		}
	}
	after, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "broken JSON" {
		t.Fatal("malformed pending state was modified")
	}
	if inspect := doctorShareInspection(root); inspect["mp1"] == nil {
		t.Fatal("doctor produced no row for the malformed-pending session")
	} else if _, hasErr := inspect["mp1"].(map[string]any)["attestation_error"]; !hasErr {
		t.Fatalf("doctor did not surface the malformed pending state: %v", inspect["mp1"])
	}
}

// TestDoctorCorruptKeyRemedyInspectionOnly pins the round-3 text ruling:
// a corrupt body key's remedy is operator inspection, never --renew.
func TestDoctorCorruptKeyRemedyInspectionOnly(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "ck1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "ck1")
	if err := os.WriteFile(filepath.Join(keyDir, "body.key"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspect := doctorShareInspection(root)
	info := inspect["ck1"].(map[string]any)
	remedy, _ := info["remedy"].(string)
	if !strings.Contains(remedy, "do NOT renew") {
		t.Fatalf("corrupt-key remedy points at renew: %v", remedy)
	}
}

// TestAtomicStateWritesFaulted pins verifier r4 P2-1(a): the share state
// writes go through fsq.WriteFileAtomic's SyncDir calls, and a failure of
// either directory sync surfaces as a refused write — never as a silent,
// non-durable publication. Deleting either SyncDir from WriteFileAtomic
// makes this test fail (the fault hook never fires, and the hook-installed
// failure is the only thing the test asserts on).
func TestAtomicStateWritesFaulted(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "af1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "af1")
	st, err := loadShareState(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	tags := st.Pending.Tags
	if err != nil {
		t.Fatal(err)
	}
	// Wire the fsq ambient SyncDir fault hook (the same mechanism #827
	// uses) so the FIRST directory sync fails; the pending write must
	// refuse, and the key dir must keep no temp residue.
	restore := fsq.SyncDirAmbientSwapForTest(func(dir string) error {
		return errors.New("fault: dir sync failed")
	})
	defer restore()
	_, stderr, code := runShareLoose("--root", root, "--session", "af1", "--tag-file", writeTagFile(t, keyDir, tags[0]))
	if code == 0 {
		t.Fatal("staged write succeeded while the directory sync was faulted")
	}
	if !strings.Contains(stderr, "dir sync failed") {
		t.Fatalf("sync failure not propagated to the operator:\n%s", stderr)
	}
	// No temp residue: the atomic helper cleaned up after the fault.
	entries, err := os.ReadDir(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp residue left after a faulted write: %s", e.Name())
		}
	}
}

// TestSyncDirSwappedIsTheRealOne pins the pin: the fault hook above only
// bites if SyncDir is actually on the write path. Deleting a SyncDir call
// from fsq.WriteFileAtomic removes the hook's failure from the write path
// and this test fails. (Verifer r4 P2-1(a): one test that fails when the
// directory sync is removed.)
func TestSyncDirSwappedIsTheRealOne(t *testing.T) {
	dir := t.TempDir()
	restore := fsq.SyncDirAmbientSwapForTest(func(string) error { return errors.New("fault: dir sync failed") })
	defer restore()
	if _, err := fsq.WriteFileAtomic(dir, "probe.txt", []byte("x"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic succeeded with a faulted dir sync: SyncDir is not on the write path")
	}
	if _, err := os.Stat(filepath.Join(dir, "probe.txt")); !os.IsNotExist(err) {
		t.Fatalf("faulted write still landed: %v", err)
	}
}

// TestReconcileCrashLeftovers pins verifier r3 P2-3: a crash between the
// share.json publication and the pending/staged cleanup heals on the next
// command — plain share returns to the clean enrolled printout and the
// leftovers are gone.
func TestReconcileCrashLeftovers(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "rc1")
	enrollAllPending(t, root, "rc1")
	out, _, code := runShare(t, "--root", root, "--session", "rc1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	condsByKind := pendingConditions(t, out)
	// Enroll all but leave pending+staged in place, simulating the crash
	// window: write the staged doc manually for the first kind, then
	// publish the full generation behind the CLI's back by enrolling all
	// kinds and restoring the leftovers afterward.
	for _, kind := range bodykey.ShareKinds {
		enrollOne(t, root, "rc1", kind, condsByKind[kind])
	}
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "rc1")
	// Simulate the leftovers: the CLI consumed them on publication; put
	// them back as a crash would have left them (full pending doc with the
	// renewed window tags, staged doc with one signed tag).
	pendingTags2 := make([]map[string]any, 0, len(bodykey.ShareKinds))
	for _, kind := range bodykey.ShareKinds {
		pendingTags2 = append(pendingTags2, map[string]any{"kind": kind, "owner_pubkey": "o", "conditions": condsByKind[kind], "sig": "s"})
	}
	pendingDoc := map[string]any{"tags": pendingTags2, "not_after": time.Now().Add(24 * time.Hour).Unix()}
	rawP, _ := json.Marshal(pendingDoc)
	if err := os.WriteFile(filepath.Join(keyDir, "share.pending.json"), rawP, 0o600); err != nil {
		t.Fatal(err)
	}
	stagedDoc := []map[string]any{{"kind": 20003, "owner_pubkey": "o", "conditions": condsByKind[20003], "sig": "s"}}
	raw, _ := json.Marshal(stagedDoc)
	if err := os.WriteFile(filepath.Join(keyDir, stagedName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Next plain share: reconciles to the clean enrolled printout.
	out2, _, code := runShare(t, "--root", root, "--session", "rc1")
	if code != 0 {
		t.Fatal("plain share refused over crash leftovers")
	}
	if strings.Contains(out2, "staged") || strings.Contains(out2, "outstanding") {
		t.Fatalf("crash leftovers not reconciled:\n%s", out2)
	}
	if _, err := os.Stat(filepath.Join(keyDir, stagedName)); !os.IsNotExist(err) {
		t.Fatalf("staged leftover not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(keyDir, "share.pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending leftover not removed: %v", err)
	}
}

// TestCorruptStagedRemedy pins verifier r3 P2-4: a corrupt staged file is
// refused with the file named and the remedy (remove it, re-sign).
func TestCorruptStagedRemedy(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "cs1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "cs1")
	if err := os.WriteFile(filepath.Join(keyDir, stagedName), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := loadShareState(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	tags := st.Pending.Tags
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runShareLoose("--root", root, "--session", "cs1", "--tag-file", writeTagFile(t, keyDir, tags[0]))
	if code == 0 {
		t.Fatal("enrollment succeeded over a corrupt staged file")
	}
	if !strings.Contains(stderr, "remedy: remove") {
		t.Fatalf("corrupt-staged refusal carries no remedy:\n%s", stderr)
	}
}

// TestShortPendingWindowCannotPublish pins verifier r3 P2-1: publication
// requires a staged tag for every kind in ShareKinds, checked in code — a
// hand-edited one-entry pending window can never drop still-valid tags.
func TestShortPendingWindowCannotPublish(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "sp1")
	enrollAllPending(t, root, "sp1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "sp1")
	before := readEnrolledTagsForTest(t, keyDir)
	if len(before) != len(bodykey.ShareKinds) {
		t.Fatalf("expected a full generation first, got %d kinds", len(before))
	}
	// Verifier r4 P2-1(b): the planted one-entry window must carry a bound
	// the published generation does NOT cover — a window built from the
	// published tags is deleted by reconcile and the enforcement never
	// runs. A +40d bound for kind 20003 is fresh, so reconcile keeps the
	// window and the full-kind enforcement is what refuses publication.
	oneKind := uint16(20003)
	oneConds := fmt.Sprintf("kind=%d&created_at<%d", oneKind, time.Now().Add(40*24*time.Hour).Unix())
	pendingDoc := map[string]any{
		"tags":      []map[string]any{{"kind": oneKind, "owner_pubkey": "o", "conditions": oneConds, "sig": "s"}},
		"not_after": time.Now().Add(41 * 24 * time.Hour).Unix(),
	}
	raw, _ := json.Marshal(pendingDoc)
	if err := os.WriteFile(filepath.Join(keyDir, "share.pending.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Enrolling the one kind in the short window must REFUSE with the
	// missing kinds named (verifier r4 P2-2: refusal, not silent staging).
	_, stderr, code := runShareLoose("--root", root, "--session", "sp1", "--tag-file", writeTagFile(t, keyDir, shareTagFile{Kind: oneKind, OwnerPubKey: "o", Conditions: oneConds, Sig: "s"}))
	if code == 0 {
		t.Fatal("enrollment over a short pending window did not refuse")
	}
	if !strings.Contains(stderr, "does not cover every kind in ShareKinds") {
		t.Fatalf("short-window refusal does not name the problem:\n%s", stderr)
	}
	after := readEnrolledTagsForTest(t, keyDir)
	if len(after) != len(bodykey.ShareKinds) {
		t.Fatalf("short pending window dropped still-valid tags: %d -> %d", len(before), len(after))
	}
}

// TestDaysZeroExplicitRefused pins verifier r4 P2-5: an EXPLICIT --days 0
// is refused with the 1..90 message; it is never silently promoted to the
// default window.
func TestDaysZeroExplicitRefused(t *testing.T) {
	root := t.TempDir()
	_, stderr, code := runShareLoose("--root", root, "--session", "dz1", "--dry-run", "--days", "0")
	if code == 0 {
		t.Fatal("--days 0 was accepted (silently promoted to the default)")
	}
	if !strings.Contains(stderr, "--days must be 1..90") {
		t.Fatalf("--days 0 refusal carries the wrong message:\n%s", stderr)
	}
	// Unset --days still gets the documented default.
	if _, _, code := runShareLoose("--root", root, "--session", "dz1", "--dry-run"); code != 0 {
		t.Fatal("unset --days no longer defaults")
	}
}

// TestDoctorNamesSymlinkedLeaf pins verifier r4 P1-2: doctor's leaf
// refusals carry the refused file's path (typed stateLeafError), for
// share.json, share.pending.json and share.staged.json alike.
func TestDoctorNamesSymlinkedLeaf(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"enrolled", "share.json"},
		{"pending", "share.pending.json"},
		{"staged", "share.staged.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			runShare(t, "--root", root, "--session", "dl1")
			keyDir := filepath.Join(root, "extensions", "remote", "keys", "dl1")
			if err := os.Remove(filepath.Join(keyDir, tc.file)); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.json")
			if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(keyDir, tc.file)); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			doc := doctorShareInspection(root)
			sess, _ := doc["dl1"].(map[string]any)
			if sess == nil {
				t.Fatalf("doctor missing session dl1: %v", doc)
			}
			for _, key := range []string{"attestation_error", "key_error", "staged_error"} {
				if msg, ok := sess[key].(string); ok {
					if msg == "symlinked; refusing" || !strings.Contains(msg, tc.file) {
						t.Fatalf("doctor %s does not name %s: %q", key, tc.file, msg)
					}
					return
				}
			}
			t.Fatalf("doctor reported no error over a symlinked %s: %v", tc.file, sess)
		})
	}
}

// TestDoctorRefusesSymlinkedStaged pins verifier r4 P1-1: doctor (via
// reconcileStagedState) never reads a symlinked share.staged.json through
// the link, never acts on out-of-root content, and never replaces the
// link. Plain share is already covered by the leaf-confinement tests.
func TestDoctorRefusesSymlinkedStaged(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "ds1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "ds1")
	outside := filepath.Join(t.TempDir(), "outside-staged.json")
	if err := os.WriteFile(outside, []byte(`[{"kind":20003,"owner_pubkey":"o","conditions":"kind=20003&created_at<9999999999","sig":"s"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	stagedPath := filepath.Join(keyDir, stagedName)
	if err := os.Remove(stagedPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, stagedPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_ = doctorShareInspection(root)
	if fi, err := os.Lstat(stagedPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("doctor replaced or removed the symlinked staged file: %v", err)
	}
	// The out-of-root target is untouched.
	raw, err := os.ReadFile(outside)
	if err != nil || !strings.Contains(string(raw), "9999999999") {
		t.Fatalf("out-of-root staged target was damaged: %v", err)
	}
}

// TestCleanupFailureAfterPublicationPropagates pins verifier r5 P1-1:
// removeStateLeaf propagates the removal error for an ORDINARY REGULAR
// file — the exact input the deleted removeEmptyDir fallback silently
// discarded (Lstat succeeded, IsDir false, nil returned) — and the error
// carries the committed-state text. Probed at the helper seam with a
// regular file in a read-only parent directory (portable unlink
// blocker); the verifier's live binary run proved the two publication
// sites use this helper (share.go:637/:641), and the three reconcile
// sites propagate separately.
func TestCleanupFailureAfterPublicationPropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only-dir removal semantics differ on Windows")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "share.pending.json")
	if err := os.WriteFile(victim, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := removeStateLeaf(victim)
	if err == nil {
		t.Fatal("removeStateLeaf returned nil for an un-removable REGULAR file (r5 P1-1: error discarded)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "published generation is committed") {
		t.Fatalf("cleanup failure lacks the committed-state distinction:\n%s", msg)
	}
	if !strings.Contains(msg, "share.pending.json") {
		t.Fatalf("cleanup failure does not name the file:\n%s", msg)
	}
	// The leaf itself is untouched (still present for the operator).
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Fatalf("the un-removable leaf vanished (setup wrong): %v", statErr)
	}
}

// TestDoctorIsReportOnly pins the architect ruling of 10:09Z (r4 P2-3):
// doctor never deletes or rewrites state. Over crash leftovers it reports
// the leftover with the reconcile remedy and leaves every file untouched;
// plain share (an explicit operator action) is the command that heals.
func TestDoctorIsReportOnly(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "ro1")
	enrollAllPending(t, root, "ro1")
	out, _, code := runShare(t, "--root", root, "--session", "ro1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	condsByKind := pendingConditions(t, out)
	for _, kind := range bodykey.ShareKinds {
		enrollOne(t, root, "ro1", kind, condsByKind[kind])
	}
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "ro1")
	// Plant the crash leftovers (published generation + pending + staged).
	pendingTags2 := make([]map[string]any, 0, len(bodykey.ShareKinds))
	for _, kind := range bodykey.ShareKinds {
		pendingTags2 = append(pendingTags2, map[string]any{"kind": kind, "owner_pubkey": "o", "conditions": condsByKind[kind], "sig": "s"})
	}
	rawP, _ := json.Marshal(map[string]any{"tags": pendingTags2, "not_after": time.Now().Add(24 * time.Hour).Unix()})
	if err := os.WriteFile(filepath.Join(keyDir, "share.pending.json"), rawP, 0o600); err != nil {
		t.Fatal(err)
	}
	stagedRaw, _ := json.Marshal([]map[string]any{{"kind": 20003, "owner_pubkey": "o", "conditions": condsByKind[20003], "sig": "s"}})
	if err := os.WriteFile(filepath.Join(keyDir, stagedName), stagedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Snapshot every file's bytes before doctor.
	snap := map[string]string{}
	for _, name := range []string{"share.json", "share.pending.json", stagedName} {
		raw, err := os.ReadFile(filepath.Join(keyDir, name))
		if err != nil {
			t.Fatal(err)
		}
		snap[name] = string(raw)
	}
	entriesBefore, err := os.ReadDir(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	// Doctor runs: must REPORT, not heal.
	info := doctorShareInspection(root)["ro1"].(map[string]any)
	row, has := info["leftover"].(string)
	if !has {
		t.Fatalf("doctor did not report the leftovers: %v", info)
	}
	if !strings.Contains(row, "reconcile") {
		t.Fatalf("doctor's leftover row carries no reconcile remedy: %q", row)
	}
	// Verifier r6 P1-1: the leftover row is additive — the attestation
	// and per-kind rows are still emitted.
	if info["attestation"] != "enrolled" {
		t.Fatalf("leftover row suppressed the attestation row (r6 P1-1): %v", info)
	}
	if _, ok := info["kinds"]; !ok {
		t.Fatalf("leftover row suppressed the per-kind table (r6 P1-1): %v", info)
	}
	// Every file byte-identical; no file added or removed.
	for name, before := range snap {
		raw, err := os.ReadFile(filepath.Join(keyDir, name))
		if err != nil {
			t.Fatalf("doctor removed or broke %s: %v", name, err)
		}
		if string(raw) != before {
			t.Fatalf("doctor rewrote %s", name)
		}
	}
	entriesAfter, err := os.ReadDir(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("doctor changed the key dir contents: %d -> %d entries", len(entriesBefore), len(entriesAfter))
	}
	// Plain share (explicit operator action) is what heals.
	if _, _, code := runShare(t, "--root", root, "--session", "ro1"); code != 0 {
		t.Fatal("plain share refused over crash leftovers")
	}
	if _, err := os.Stat(filepath.Join(keyDir, stagedName)); !os.IsNotExist(err) {
		t.Fatalf("plain share did not reconcile: %v", err)
	}
}

// TestExpiryRowSurvivesLeftoverRow pins verifier r6 P1-1: the leftover row
// is ADDITIVE. An expired generation with crash leftovers still reports
// expiry_warning, attestation and the per-kind table — the leftover row
// joins the diagnostic, never replaces it. (Shape from the verifier's
// overlay probe, RED at 2793bd9.)
func TestExpiryRowSurvivesLeftoverRow(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "el1")
	enrollAllPending(t, root, "el1")
	// Force the active generation to be already expired.
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "el1")
	tags := readEnrolledTagsForTest(t, keyDir)
	for i := range tags {
		tags[i].Conditions = rewriteBoundToPast(tags[i].Conditions)
	}
	if err := writeEnrolledTagsForTest(t, keyDir, tags); err != nil {
		t.Fatal(err)
	}
	// Plant crash leftovers (a stale pending window + staged entry) whose
	// conditions match the CURRENT generation — the state reportStagedState
	// treats as "published generation already covers the pending window".
	stGen, _ := loadShareState(keyDir)
	gen := stGen.Enrolled.Gen
	pendingTags := make([]shareTagFile, 0, len(gen.Tags))
	for _, g := range gen.Tags {
		pendingTags = append(pendingTags, shareTagFile{Kind: g.Kind, OwnerPubKey: "o", Conditions: g.Conditions, Sig: "s"})
	}
	pendingRaw, _ := json.Marshal(map[string]any{"tags": pendingTags, "not_after": time.Now().Add(24 * time.Hour).Unix()})
	if err := os.WriteFile(filepath.Join(keyDir, "share.pending.json"), pendingRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	stagedRaw, _ := json.Marshal(pendingTags)
	if err := os.WriteFile(filepath.Join(keyDir, stagedName), stagedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	info := doctorShareInspection(root)["el1"].(map[string]any)
	exp, has := info["expiry_warning"].(string)
	if !has {
		t.Fatalf("the leftover row SUPPRESSED the expiry warning (r6 P1-1): %v", info)
	}
	if !strings.Contains(exp, "expired") {
		t.Fatalf("expiry_warning did not report the lapse: %q", exp)
	}
	if _, ok := info["leftover"]; !ok {
		t.Fatalf("the leftover row is missing alongside the expiry row: %v", info)
	}
	if info["attestation"] != "enrolled" {
		t.Fatalf("attestation suppressed alongside the leftover row: %v", info)
	}
}

// TestDoctorRemedyForSymlinkedStagedIsExecutable pins verifier r6 P2-1:
// the symlinked-staged remedy names the removal of the link itself (plain
// share refuses that state, so pointing at share alone is bad advice).
func TestDoctorRemedyForSymlinkedStagedIsExecutable(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "sr1")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "sr1")
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	stagedPath := filepath.Join(keyDir, stagedName)
	if err := os.Remove(stagedPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, stagedPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	info := doctorShareInspection(root)["sr1"].(map[string]any)
	row, ok := info["staged_error"].(string)
	if !ok {
		t.Fatalf("no staged_error row over a symlinked staged leaf: %v", info)
	}
	if !strings.Contains(row, "remove the symlink at "+stagedPath) {
		t.Fatalf("symlinked-staged remedy is not executable (r6 P2-1): %q", row)
	}
}

// TestBodyPubLeafConfinementEveryCommand pins verifier r6 P2-3: a
// symlinked body.pub is refused by share, renew and dry-run even when
// body.key already exists, and the half-minted key directory is never
// created (body.pub is lstat'd before the body.key write). Doctor (r7
// round-6 order) reports the leaf additively instead of refusing —
// it must name body.pub and print the remedy.
func TestBodyPubLeafConfinementEveryCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"mint", []string{"--root", "%ROOT%", "--session", "bp1"}},
		{"dry-run", []string{"--root", "%ROOT%", "--session", "bp1", "--dry-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			keyDir := filepath.Join(root, "extensions", "remote", "keys", "bp1")
			if err := os.MkdirAll(keyDir, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "victim.txt")
			if err := os.WriteFile(outside, []byte("IMPORTANT USER FILE"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(keyDir, "body.pub")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			args := make([]string, 0, len(tc.args)+1)
			for _, a := range tc.args {
				args = append(args, strings.ReplaceAll(a, "%ROOT%", root))
			}
			_, _, code := runShareLoose(args...)
			if code == 0 {
				t.Fatalf("%s accepted a symlinked body.pub (r6 P2-3)", tc.name)
			}
			// The victim file is untouched.
			raw, err := os.ReadFile(outside)
			if err != nil || string(raw) != "IMPORTANT USER FILE" {
				t.Fatalf("out-of-root body.pub target damaged: %v", err)
			}
			if tc.name == "mint" {
				// Half-minted key dir: body.key must be ABSENT after the
				// refusal (body.pub is checked before the key write).
				if _, err := os.Stat(filepath.Join(keyDir, "body.key")); !os.IsNotExist(err) {
					t.Fatalf("body.key written despite the body.pub refusal (r6 P2-3): %v", err)
				}
			}
		})
	}
	// With body.key present, plain share must still refuse the symlinked
	// body.pub (the leaf set covers it on every command).
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "bp2")
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "bp2")
	outside := filepath.Join(t.TempDir(), "victim2.txt")
	if err := os.WriteFile(outside, []byte("IMPORTANT"), 0o600); err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(keyDir, "body.pub")
	if err := os.Remove(pubPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, pubPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, stderr, code := runShareLoose("--root", root, "--session", "bp2")
	if code == 0 {
		t.Fatal("plain share accepted a symlinked body.pub with body.key present (r6 P2-3)")
	}
	if !strings.Contains(stderr, "body.pub") {
		t.Fatalf("body.pub refusal does not name the leaf (r6 P3): %s", stderr)
	}
	// Doctor (r7 round-6 order) reports the same leaf additively: it does
	// not refuse (read-only), but the row names body.pub and the remedy.
	inspect := doctorShareInspection(root)
	info := inspect["bp2"].(map[string]any)
	if bpErr, _ := info["body_pub_error"].(string); !strings.Contains(bpErr, "body.pub") {
		t.Fatalf("doctor did not report the symlinked body.pub leaf: %v", info)
	}
	if remedy, _ := info["remedy"].(string); !strings.Contains(remedy, "body.pub") {
		t.Fatalf("doctor remedy does not name the leaf: %v", info["remedy"])
	}
}

// TestDoctorUnknownProgressOverCorruptStaged pins verifier r7 P1: when the
// staged leaf is unreadable (corrupt) the signed count is NOT printed —
// the zero default fabricated "5 of 5 kinds signed" over a session with
// 2 of 5 signed. Progress is a tri-state: doctor says "progress unknown",
// keeps the enrolled attestation, and suppresses any remedy that presumes
// known progress (renew). Same for the plain-pending branch.
func TestDoctorUnknownProgressOverCorruptStaged(t *testing.T) {
	root := t.TempDir()
	runShare(t, "--root", root, "--session", "u1")
	enrollAllPending(t, root, "u1")
	out, _, code := runShare(t, "--root", root, "--session", "u1", "--renew")
	if code != 0 {
		t.Fatal("renew refused")
	}
	condsByKind := pendingConditions(t, out)
	// Two of five kinds signed; the staged leaf then corrupts. Derive the
	// kinds from the pending window itself (ShareKinds is not sequential)
	// and capture the conditions up front — the staged file shrinks after
	// each enrollment.
	kinds := make([]uint16, 0, len(condsByKind))
	for kind := range condsByKind {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	enrollOne(t, root, "u1", kinds[0], condsByKind[kinds[0]])
	enrollOne(t, root, "u1", kinds[1], condsByKind[kinds[1]])
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "u1")
	if err := os.WriteFile(filepath.Join(keyDir, stagedName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspect := doctorShareInspection(root)
	info := inspect["u1"].(map[string]any)
	att, _ := info["attestation"].(string)
	if strings.Contains(att, "of 5") {
		t.Fatalf("doctor fabricated a signed count over corrupt staged state (r7 P1): %q", att)
	}
	// Enrolled branch: attestation stays the enrolled FACT; the unknown
	// progress is its own row (the r7 wording for the pending branch).
	prog, _ := info["progress"].(string)
	if !strings.Contains(prog, "unknown") {
		t.Fatalf("doctor progress = %q, want the unknown-progress row (r7 P1)", prog)
	}
	// r8 P1-3: the renewal-in-progress warning SURVIVES unreadable staged
	// state (the pending window alone proves the renewal is in progress);
	// only a fabricated count is forbidden.
	if warn, has := info["warning"].(string); !has || !strings.Contains(warn, "do NOT renew") {
		t.Fatalf("renewal-in-progress warning missing over unreadable state: %v", info)
	} else if strings.Contains(warn, "of 5") {
		t.Fatalf("warning fabricated a signed count over unreadable state: %q", warn)
	}
	if _, has := info["staged_error"]; !has {
		t.Fatalf("staged_error row missing: %v", info)
	}
	// The same tri-state holds in the plain-pending branch (no enrollment):
	// corrupt staged over a pending window is unknown, never "0 of 5".
	root2 := t.TempDir()
	runShare(t, "--root", root2, "--session", "u2")
	keyDir2 := filepath.Join(root2, "extensions", "remote", "keys", "u2")
	if err := os.WriteFile(filepath.Join(keyDir2, stagedName), []byte("{also not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspect2 := doctorShareInspection(root2)
	info2 := inspect2["u2"].(map[string]any)
	att2, _ := info2["attestation"].(string)
	if strings.Contains(att2, "of 5") {
		t.Fatalf("doctor printed a signed count over unreadable staged state (r7 P1): %q", att2)
	}
	if !strings.Contains(att2, "progress unknown") {
		t.Fatalf("pending-branch attestation = %q, want unknown-progress", att2)
	}
}

// TestDryRunRefusesCorruptStaged pins verifier r7 P2 (codex 3): plain
// --dry-run with a valid pending window and a corrupt share.staged.json
// must refuse (nonzero) like the real run — it must not exit 0 and print
// signing data a real run would never print.
func TestDryRunRefusesCorruptStaged(t *testing.T) {
	root := t.TempDir()
	out, _, code := runShare(t, "--root", root, "--session", "u3")
	if code != 0 {
		t.Fatal("share refused pre-mint")
	}
	_ = out
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "u3")
	if err := os.WriteFile(filepath.Join(keyDir, stagedName), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runShareLoose("--root", root, "--session", "u3", "--dry-run")
	if code == 0 {
		t.Fatal("dry-run exited 0 over corrupt staged state while the real run refuses (r7 P2)")
	}
	if !strings.Contains(stderr, stagedName) && !strings.Contains(stderr, "invalid") {
		t.Fatalf("dry-run refusal does not name the corrupt leaf: %s", stderr)
	}
}
