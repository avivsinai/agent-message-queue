package bodykey

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// NIP-OA test vector (docs/nips/NIP-OA.md): the compliance anchor.
const (
	vecOwnerSecret = "0000000000000000000000000000000000000000000000000000000000000001"
	vecAgentPubkey = "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	vecConditions  = "kind=1&created_at<1713957000"
	vecSHA         = "08cdecd55af4c28d3801fd69615dcf5cc04fab3bc134b38a840bf157197069a6"
	vecSig         = "8b7df2575caf0a108374f8471722b233c53f9ff827a8b0f91861966c3b9dd5cb2e189eae9f49d72187674c2f5bd244145e10ff86c9f257ffe65a1ee5f108b369"
	vecOwnerPub    = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
)

func mustHexSecret(t *testing.T, s string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return [32]byte(raw)
}

// TestNIPAOVector_Interoperable pins compliance the honest way. BIP340
// signatures are non-unique: the nonce derives from auxiliary randomness the
// signer chooses, so byte-equality with the NIP's published signature is not
// a property any conformant signer can claim (the vector's own aux is not
// derivable — verified by reproducing the challenge recovery: the vector
// signature verifies under btcec's independent BIP340 verifier, and our
// signatures do too). The compliance assertions that DO hold and are pinned:
// (1) our preimage hash equals the NIP's published sha256(preimage);
// (2) the NIP's published tag verifies under our Verify;
// (3) our minted signature verifies under the library's independent verifier
// — the same verifier class a relay uses.
func TestNIPAOVector_Interoperable(t *testing.T) {
	owner := mustHexSecret(t, vecOwnerSecret)
	tag, err := SignAuthTag(owner, vecAgentPubkey, vecConditions)
	if err != nil {
		t.Fatal(err)
	}
	// (1) preimage hash byte-for-byte with the NIP.
	if got := hex.EncodeToString(tag.Preimage(vecAgentPubkey)); got != vecSHA {
		t.Fatalf("preimage sha = %s, want %s", got, vecSHA)
	}
	if got := tag.OwnerPubKey; got != vecOwnerPub {
		t.Fatalf("owner pubkey = %s, want %s", got, vecOwnerPub)
	}
	// (2) the NIP's published tag passes our verification.
	published, err := ParseAuthTag([]string{"auth", vecOwnerPub, vecConditions, vecSig})
	if err != nil {
		t.Fatal(err)
	}
	if err := published.Verify(vecAgentPubkey); err != nil {
		t.Fatalf("NIP vector tag fails our Verify: %v", err)
	}
	// (3) our signature passes the library's independent BIP340 verifier.
	ownerXOnly, err := hex.DecodeString(tag.OwnerPubKey)
	if err != nil {
		t.Fatal(err)
	}
	ownerKey, err := schnorr.ParsePubKey(ownerXOnly)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := schnorr.ParseSignature(tag.Sig[:])
	if err != nil {
		t.Fatalf("our signature is not parseable BIP340: %v", err)
	}
	if !sig.Verify(tag.Preimage(vecAgentPubkey), ownerKey) {
		t.Fatal("our signature fails the independent BIP340 verifier")
	}
	// Conditions semantics per the vector event.
	if err := tag.Satisfies(1, 1713956400); err != nil {
		t.Fatalf("satisfies: %v", err)
	}
	if err := tag.Satisfies(1, 1713957000); err == nil {
		t.Fatal("created_at == bound accepted, want strict <")
	}
	if err := tag.Satisfies(2, 1713956400); err == nil {
		t.Fatal("kind=2 accepted under kind=1 clause")
	}
}

// TestNIPAOVector_Parse verifies ParseAuthTag on the vector's tag array and
// the malformed rejections the NIP lists.
func TestNIPAOVector_Parse(t *testing.T) {
	tag, err := ParseAuthTag([]string{"auth", vecOwnerPub, vecConditions, vecSig})
	if err != nil {
		t.Fatal(err)
	}
	if err := tag.Verify(vecAgentPubkey); err != nil {
		t.Fatalf("verify: %v", err)
	}

	for name, tt := range map[string][]string{
		"three elements":   {"auth", vecOwnerPub, vecConditions, vecSig[:64]},
		"five elements":    {"auth", vecOwnerPub, vecConditions, vecSig, "extra"},
		"wrong name":       {"aut", vecOwnerPub, vecConditions, vecSig},
		"bad sig hex":      {"auth", vecOwnerPub, vecConditions, strings.Repeat("zz", 64)},
		"trailing &":       {"auth", vecOwnerPub, "kind=1&", vecSig},
		"leading zero":     {"auth", vecOwnerPub, "kind=01", vecSig},
		"self attestation": {"auth", vecAgentPubkey, vecConditions, vecSig},
	} {
		_, err := ParseAuthTag(tt)
		if name == "three elements" || name == "five elements" || name == "wrong name" || name == "bad sig hex" {
			if err == nil {
				t.Fatalf("%s: ParseAuthTag accepted, want error", name)
			}
			continue
		}
		// Structural parse succeeds for condition/self-attestation cases;
		// Verify/ParseConditions must reject them.
		parsed, perr := ParseAuthTag(tt)
		if perr != nil {
			continue
		}
		if name == "self attestation" {
			if verr := parsed.Verify(vecAgentPubkey); verr == nil {
				t.Fatalf("%s: Verify accepted, want error", name)
			}
			continue
		}
		if verr := parsed.Verify(vecAgentPubkey); verr == nil {
			t.Fatalf("%s: Verify accepted, want error", name)
		}
	}
}

// TestParseConditions_Strict covers the NIP's conditions grammar.
func TestParseConditions_Strict(t *testing.T) {
	valid := map[string]bool{
		"":                             true,
		"kind=1":                       true,
		"kind=0":                       true,
		"kind=65535":                   true,
		"created_at<4294967295":        true,
		"created_at>0":                 true,
		"kind=1&created_at<1713957000": true,
	}
	for c, want := range valid {
		if _, err := ParseConditions(c); (err == nil) != want {
			t.Fatalf("ParseConditions(%q) err=%v, want valid=%v", c, err, want)
		}
	}
	invalid := []string{
		"kind=1&",               // trailing delimiter
		"&kind=1",               // leading delimiter
		"kind=1&&created_at<1",  // double delimiter
		"kind=01",               // leading zero
		"kind=65536",            // out of range
		"created_at<4294967296", // out of range
		" kind=1",               // whitespace
		"kind=1 ",               // trailing whitespace
		"kind =1",               // internal whitespace
		"foo=1",                 // unsupported clause
		"kind=",                 // empty value
		"kind=1x",               // non-digit
	}
	for _, c := range invalid {
		if _, err := ParseConditions(c); err == nil {
			t.Fatalf("ParseConditions(%q) accepted, want error", c)
		}
	}
}

// TestBodyKeyFileLifecycle covers mint-once, mode enforcement, format
// rejection, and the public-line cross-check.
func TestBodyKeyFileLifecycle(t *testing.T) {
	dir := t.TempDir()
	k, err := Mint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Mint(dir); err == nil {
		t.Fatal("second mint accepted; want mint-once refusal")
	}
	if info, err := os.Stat(filepath.Join(dir, "body.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("body.key perm = %v err = %v, want 0600", info.Mode().Perm(), err)
	}
	// Round-trip load.
	loaded, err := Load(filepath.Join(dir, "body.key"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PublicKeyHex() != k.PublicKeyHex() {
		t.Fatal("round-trip pubkey mismatch")
	}
	// The public line must match when present.
	content, _ := os.ReadFile(filepath.Join(dir, "body.key"))
	withPub := strings.TrimRight(string(content), "\n") + "\npublic " + k.PublicKeyHex() + "\n"
	if _, err := parse(withPub); err != nil {
		t.Fatalf("public line match rejected: %v", err)
	}
	badPub := strings.Replace(withPub, k.PublicKeyHex(), strings.Repeat("ab", 32), 1)
	if _, err := parse(badPub); err == nil {
		t.Fatal("mismatched public line accepted")
	}
	// A bridge HostKey file (generation/seed lines) is rejected as wrong format.
	hostKey := "generation g1\nseed " + strings.Repeat("11", 32) + "\n"
	if _, err := parse(hostKey); err == nil || !strings.Contains(err.Error(), ErrWrongFormat.Error()) {
		t.Fatalf("host key format err = %v, want ErrWrongFormat", err)
	}
	// A zero scalar is out of range.
	zero := "secret " + strings.Repeat("00", 32) + "\n"
	if _, err := parse(zero); err == nil {
		t.Fatal("zero scalar accepted")
	}
}

// TestShareConditionsAndRenew covers the share tag shape and renewal:
// a renewed tag (later bound) satisfies events the old one rejects.
func TestShareConditionsAndRenew(t *testing.T) {
	owner := mustHexSecret(t, vecOwnerSecret)
	dir := t.TempDir()
	k, err := LoadOrMint(dir)
	if err != nil {
		t.Fatal(err)
	}
	oldTag, err := SignAuthTag(owner, k.PublicKeyHex(), ShareConditions(1713957000))
	if err != nil {
		t.Fatal(err)
	}
	if err := oldTag.Satisfies(20003, 1713956999); err != nil {
		t.Fatalf("courier kind within window: %v", err)
	}
	if err := oldTag.Satisfies(20003, 1713957000); err == nil {
		t.Fatal("event at the bound accepted; want strict <")
	}
	if err := oldTag.Satisfies(1, 1713956999); err == nil {
		t.Fatal("kind=1 accepted; share tag does not authorize it")
	}
	// Renewal re-prints the preimage with a later bound.
	newTag, err := SignAuthTag(owner, k.PublicKeyHex(), ShareConditions(1713999999))
	if err != nil {
		t.Fatal(err)
	}
	if err := newTag.Satisfies(24200, 1713957000); err != nil {
		t.Fatalf("renewed tag admits the event the old one refused: %v", err)
	}
}
