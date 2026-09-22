package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
)

// TestTrustAddProvisionsFromIdentityPublicOutput is the ug3 happy path:
// given a fresh destination root, the operator runs `identity public` on the
// source, pipes that output to `trust add` on the destination, and the
// written trusted file authenticates under LoadTrusted with no hand-written
// file. Reverting ParsePublicIdentity to the pre-fix two-line-only parser
// reproduces the bead's observed symptom here ("identity record line 1 is
// invalid"); removing the production WriteTrusted call fails the round-trip.
func TestTrustAddProvisionsFromIdentityPublicOutput(t *testing.T) {
	// Source: a real identity, and the exact bytes `identity public` prints.
	srcRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, srcRoot, "src-mac")
	srcKey := testHostKey("src-mac", "g7")
	if err := bridge.WriteIdentity(srcRoot, srcKey); err != nil {
		t.Fatal(err)
	}
	publicOutput := fmt.Sprintf("host=src-mac generation=%s public=%x\n", srcKey.Generation, srcKey.Public())

	// Destination: fresh root, pipe the record through trust add. No
	// --host: the record's host field is authoritative (review-845-r1
	// P2-1) because apply-file looks the trust file up by it.
	dstRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, dstRoot, "dst-mac")
	if err := runTrustAdd([]string{"--root", dstRoot}, strings.NewReader(publicOutput)); err != nil {
		t.Fatalf("runTrustAdd: %v", err)
	}

	got, generation, err := bridge.LoadTrusted(dstRoot, "src-mac")
	if err != nil {
		t.Fatalf("LoadTrusted after provisioning: %v", err)
	}
	if generation != srcKey.Generation {
		t.Fatalf("trusted generation = %q, want %q", generation, srcKey.Generation)
	}
	if string(got) != string(srcKey.Public()) {
		t.Fatalf("trusted public key mismatch")
	}
	// File mode must be private (writePrivateFile contract, 0600).
	info, err := os.Lstat(bridge.TrustedPath(dstRoot, "src-mac"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("trusted file mode = %o, want 600", perm)
	}
}

// TestTrustAddAcceptsKeyFileForm pins that the two-line record the loader
// itself accepts ("generation <g>" / "public <hex>") provisions too — the
// same command must take either shape. This shape carries no host field,
// so --host is required and used.
func TestTrustAddAcceptsKeyFileForm(t *testing.T) {
	key := testHostKey("src-two", "g9")
	record := fmt.Sprintf("generation %s\npublic %x\n", key.Generation, key.Public())
	dstRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, dstRoot, "dst-mac")
	if err := runTrustAdd([]string{"--root", dstRoot, "--host", "src-two"}, strings.NewReader(record)); err != nil {
		t.Fatalf("runTrustAdd: %v", err)
	}
	got, generation, err := bridge.LoadTrusted(dstRoot, "src-two")
	if err != nil {
		t.Fatalf("LoadTrusted: %v", err)
	}
	if generation != "g9" || string(got) != string(key.Public()) {
		t.Fatalf("trusted = (%x, %s), want the provisioned key", got, generation)
	}
}

// TestTrustAddHostParity pins review-845-r1 P2-1: an explicit --host that
// disagrees with the record's host= field is refused (before the fix it
// wrote a dead trust record under the wrong name and exited 0), and the
// --from file path provisions with the record's own host.
func TestTrustAddHostParity(t *testing.T) {
	key := testHostKey("src-file", "g1")
	record := fmt.Sprintf("host=src-file generation=%s public=%x\n", key.Generation, key.Public())
	from := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(from, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	// --from file, no --host: provisions under the record's host.
	dstRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, dstRoot, "dst-mac")
	if err := runTrustAdd([]string{"--root", dstRoot, "--from", from}, nil); err != nil {
		t.Fatalf("runTrustAdd --from: %v", err)
	}
	if _, _, err := bridge.LoadTrusted(dstRoot, "src-file"); err != nil {
		t.Fatalf("LoadTrusted: %v", err)
	}

	// Matching --host is accepted; disagreeing --host is refused and
	// writes nothing.
	dstRoot2 := newBridgeRoot(t, "claude")
	ensureHostID(t, dstRoot2, "dst-mac")
	if err := runTrustAdd([]string{"--root", dstRoot2, "--host", "src-file"}, strings.NewReader(record)); err != nil {
		t.Fatalf("runTrustAdd matching host: %v", err)
	}
	root3 := newBridgeRoot(t, "claude")
	ensureHostID(t, root3, "dst-mac")
	if err := runTrustAdd([]string{"--root", root3, "--host", "peer-mac"}, strings.NewReader(record)); err == nil {
		t.Fatal("runTrustAdd with disagreeing --host = nil, want refusal")
	}
	if _, err := os.Lstat(bridge.TrustedPath(root3, "peer-mac")); !os.IsNotExist(err) {
		t.Fatalf("dead trust record written under wrong name: %v", err)
	}
}

// TestTrustAddRotateReplacesAtomicallyNoDowngrade pins review-845-r1 P1-1:
// rotation goes through --replace, lands atomically (the file is replaced
// in place, no partial body), and refuses a generation downgrade. Without
// --replace, re-provisioning an existing host fails with a clear error.
func TestTrustAddRotateReplacesAtomicallyNoDowngrade(t *testing.T) {
	key1 := testHostKey("src-rot", "1")
	dstRoot := newBridgeRoot(t, "claude")
	ensureHostID(t, dstRoot, "dst-mac")
	record1 := fmt.Sprintf("host=src-rot generation=%s public=%x\n", key1.Generation, key1.Public())
	if err := runTrustAdd([]string{"--root", dstRoot}, strings.NewReader(record1)); err != nil {
		t.Fatalf("initial runTrustAdd: %v", err)
	}

	// No --replace: refused, original record untouched.
	if err := runTrustAdd([]string{"--root", dstRoot}, strings.NewReader(record1)); err == nil {
		t.Fatal("re-provision without --replace = nil, want refusal")
	}
	got, generation, err := bridge.LoadTrusted(dstRoot, "src-rot")
	if err != nil || string(got) != string(key1.Public()) || generation != "1" {
		t.Fatalf("original record disturbed by refused re-provision: (%x, %s, %v)", got, generation, err)
	}

	// --replace with a NEWER generation: rotated in place.
	key2 := testHostKey("src-rot", "2")
	record2 := fmt.Sprintf("host=src-rot generation=%s public=%x\n", key2.Generation, key2.Public())
	if err := runTrustAdd([]string{"--root", dstRoot, "--replace"}, strings.NewReader(record2)); err != nil {
		t.Fatalf("runTrustAdd --replace: %v", err)
	}
	got, generation, err = bridge.LoadTrusted(dstRoot, "src-rot")
	if err != nil || string(got) != string(key2.Public()) || generation != "2" {
		t.Fatalf("rotation did not land: (%x, %s, %v)", got, generation, err)
	}
	info, err := os.Lstat(bridge.TrustedPath(dstRoot, "src-rot"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("rotated file mode = %o, want 600", perm)
	}

	// --replace with an OLDER generation: refused, g2 stays.
	if err := runTrustAdd([]string{"--root", dstRoot, "--replace"}, strings.NewReader(record1)); err == nil {
		t.Fatal("downgrade --replace = nil, want refusal")
	}
	if _, generation, err = bridge.LoadTrusted(dstRoot, "src-rot"); err != nil || generation != "2" {
		t.Fatalf("downgrade disturbed the active generation: (%s, %v)", generation, err)
	}
}

// TestTrustAddRotateCrossesDigitBoundary pins review-845-r2 P1: the
// downgrade guard compares decimal generations numerically, not as byte
// strings — the legitimate 9 -> 10 rotation is accepted and the 10 -> 9
// downgrade is refused. A byte-wise compare fails both halves of this
// test.
func TestTrustAddRotateCrossesDigitBoundary(t *testing.T) {
	key9 := testHostKey("src-num", "9")
	key10 := testHostKey("src-num", "10")
	record9 := fmt.Sprintf("host=src-num generation=%s public=%x\n", key9.Generation, key9.Public())
	record10 := fmt.Sprintf("host=src-num generation=%s public=%x\n", key10.Generation, key10.Public())

	root := newBridgeRoot(t, "claude")
	ensureHostID(t, root, "dst-mac")
	if err := runTrustAdd([]string{"--root", root}, strings.NewReader(record9)); err != nil {
		t.Fatalf("provision at 9: %v", err)
	}
	if err := runTrustAdd([]string{"--root", root, "--replace"}, strings.NewReader(record10)); err != nil {
		t.Fatalf("rotate 9 -> 10: %v (byte-compare inverts here)", err)
	}
	if _, generation, err := bridge.LoadTrusted(root, "src-num"); err != nil || generation != "10" {
		t.Fatalf("after 9 -> 10: (%s, %v)", generation, err)
	}
	if err := runTrustAdd([]string{"--root", root, "--replace"}, strings.NewReader(record9)); err == nil {
		t.Fatal("downgrade 10 -> 9 = nil, want refusal")
	}
	if _, generation, err := bridge.LoadTrusted(root, "src-num"); err != nil || generation != "10" {
		t.Fatalf("after refused 10 -> 9: (%s, %v)", generation, err)
	}

	// Non-decimal label classes have no defined order: --replace refuses
	// to guess rather than byte-comparing. (Identical labels are an
	// idempotent re-provision, not an ordering question.)
	rootWord := newBridgeRoot(t, "claude")
	ensureHostID(t, rootWord, "dst-mac")
	keyAlpha := testHostKey("src-alpha", "alpha")
	recAlpha := fmt.Sprintf("host=src-alpha generation=%s public=%x\n", keyAlpha.Generation, keyAlpha.Public())
	keyBeta := testHostKey("src-alpha", "beta")
	recBeta := fmt.Sprintf("host=src-alpha generation=%s public=%x\n", keyBeta.Generation, keyBeta.Public())
	if err := runTrustAdd([]string{"--root", rootWord}, strings.NewReader(recAlpha)); err != nil {
		t.Fatalf("provision alpha: %v", err)
	}
	if err := runTrustAdd([]string{"--root", rootWord, "--replace"}, strings.NewReader(recBeta)); err == nil || !strings.Contains(err.Error(), "cannot order") {
		t.Fatalf("unordered --replace = %v, want 'cannot order' refusal", err)
	}
	if _, generation, err := bridge.LoadTrusted(rootWord, "src-alpha"); err != nil || generation != "alpha" {
		t.Fatalf("unordered --replace disturbed the active generation: (%s, %v)", generation, err)
	}
}

// TestTrustAddRejectsUnparseableRecord reproduces the bead's observed
// defect class: a record the trusted-file parser rejects ("line 1 is
// invalid") must be refused by trust add too, with the parser's honest
// error — never accepted, and never a file written. The bad-host subtest
// pins the traversal boundary with a record whose ONLY defect is the host
// alias (review-845-r2 P2-2: the host check must run, not be pre-empted
// by an invalid key — the host value becomes a file name under
// bridge/trusted/).
func TestTrustAddRejectsUnparseableRecord(t *testing.T) {
	key := testHostKey("src-x", "g1")
	// Two-line record (no host field) so --host is required and the given
	// alias is the only defect: it reaches validateBridgeIdentifier inside
	// WriteTrusted, which refuses it before any file is written.
	validRecord := fmt.Sprintf("generation %s\npublic %x\n", key.Generation, key.Public())
	for name, tc := range map[string]struct {
		host    string
		data    string
		wantMsg string
	}{
		"bad-record": {host: "src-x", data: "not a record at all\n", wantMsg: "line 1 is invalid"},
		"bad-host":   {host: "../x", data: validRecord, wantMsg: "trusted source host"},
	} {
		t.Run(name, func(t *testing.T) {
			root := newBridgeRoot(t, "claude")
			ensureHostID(t, root, "dst-mac")
			err := runTrustAdd([]string{"--root", root, "--host", tc.host}, strings.NewReader(tc.data))
			if err == nil {
				t.Fatalf("runTrustAdd(%s) = nil, want refusal", name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("runTrustAdd(%s) error = %q, want it to name %q", name, err, tc.wantMsg)
			}
			if _, err := os.Lstat(bridge.TrustedPath(root, tc.host)); !os.IsNotExist(err) {
				t.Fatalf("trusted file written despite refusal: %v", err)
			}
		})
	}
}
