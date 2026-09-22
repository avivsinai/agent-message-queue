// Package bodykey implements the body-key contract of the AMQ Remote design
// (v3 amended, §7.5 "Key contract") and the NIP-OA Owner Attestation tag
// (buzz docs/nips/NIP-OA.md) that the owner uses to attest a body key.
//
// BodyKey is the remote-owned secp256k1 (BIP340) keypair minted per shared
// session, stored at <root>/extensions/remote/keys/<session>/body.key
// (0600) with body.pub beside it. The format is deliberately distinct from
// the bridge identity format in internal/bridge/auth.go: a 0600 file of
// whitespace-separated lines — `secret <64-char hex BIP340 scalar>` plus an
// optional `public <64-char hex>` line — validated as scalar ∈ [1, n−1]
// with the derived x-only pubkey checked against the optional `public`
// line. Interchange with HostKey is a compile-time type error.
//
// NIP-OA: the owner signs SHA256("nostr:agent-auth:" || body-pubkey || ":"
// || conditions) with their own key; the tag is
// ["auth", owner-pubkey, conditions, sig]. Conditions are strict: empty, or
// `clause&clause&...` where clause is `kind=<n>` (0–65535, canonical base
// 10), `created_at<t>` or `created_at>t` (0–4294967295). No whitespace, no
// trailing/leading '&', no '&&', no leading zeros. The conditions string is
// part of the signed preimage and is never normalized.
package bodykey

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// curveOrder is the secp256k1 group order N; a BIP340 scalar must lie in
// [1, N−1]. PrivKeyFromBytes REDUCES values mod N instead of rejecting
// them, so an out-of-range raw scalar would silently alias to a different
// key — every scalar crossing this package is range-checked explicitly.
var curveOrder = btcec.S256().N

// AuthPreimagePrefix is the exact NIP-OA domain separator.
const AuthPreimagePrefix = "nostr:agent-auth:"

// Bounds on NIP-OA condition values per the NIP: kind clauses are 0–65535,
// created_at clauses are 0–4294967295.
const (
	maxKindValue      = 65535
	maxCreatedAtValue = 4294967295
	hexSecretLen      = 64
	hexPubkeyLen      = 64
	hexSigLen         = 128
)

// BodyKey is a loaded secp256k1 body keypair. It is a distinct type from the
// bridge HostKey so the two identity formats cannot be interchanged.
type BodyKey struct {
	secret [32]byte
	pub    btcec.PublicKey
}

// Secret exposes the raw scalar (for signing the AUTH presentation and the
// courier's seal); it never crosses argv, logs, or output.
func (k *BodyKey) Secret() [32]byte { return k.secret }

// PublicKeyHex returns the 64-char lowercase x-only public key hex.
func (k *BodyKey) PublicKeyHex() string {
	return hex.EncodeToString(schnorr.SerializePubKey(&k.pub))
}

// ErrWrongFormat distinguishes a file that is not a body key at all (e.g. a
// bridge HostKey file) from other load failures.
var ErrWrongFormat = errors.New("not a body key file")

// Load reads and validates a body key file. The scalar must be a 64-char
// lowercase hex BIP340 scalar in [1, n−1]; the optional `public` line, when
// present, must equal the derived x-only pubkey.
// errSymlinkedKey refuses a symlinked key path: the key material must live
// at the owned path inside the root, never behind a link that can point
// outside it (verifier P1-1: Load/Stat follow symlinks and would adopt an
// out-of-root key silently).
var errSymlinkedKey = errors.New("body key path is a symlink; refusing")

// lstatKeyLeaf Lstats the key path: it must be a regular file (or absent).
func lstatKeyLeaf(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", errSymlinkedKey, path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("body key path %s is not a regular file", path)
	}
	return nil
}

func Load(path string) (*BodyKey, error) {
	if err := lstatKeyLeaf(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("body key file %s must be mode 0600 (has %o)", path, uint32(info.Mode().Perm()))
	}
	return parse(string(data))
}

func parse(content string) (*BodyKey, error) {
	secretSet := false
	publicLine := ""
	k := &BodyKey{}
	for _, line := range strings.FieldsFunc(content, func(r rune) bool { return r == '\n' || r == '\r' }) {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "secret":
			if secretSet {
				return nil, fmt.Errorf("%w: duplicate secret line", ErrWrongFormat)
			}
			if len(fields[1]) != hexSecretLen {
				return nil, fmt.Errorf("%w: secret must be %d hex chars", ErrWrongFormat, hexSecretLen)
			}
			// The file format is lowercase hex (writeSecretFile emits only
			// lowercase); reject uppercase so the on-disk contract stays
			// canonical (codex P2: one consistent parser contract).
			if strings.ToLower(fields[1]) != fields[1] {
				return nil, fmt.Errorf("%w: secret hex must be lowercase", ErrWrongFormat)
			}
			raw, err := hex.DecodeString(fields[1])
			if err != nil {
				return nil, fmt.Errorf("%w: secret is not hex: %v", ErrWrongFormat, err)
			}
			if err := validateScalar(raw); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrWrongFormat, err)
			}
			// PrivKeyFromBytes also returns the derived pubkey, which the
			// optional `public` line must match.
			_, pub := btcec.PrivKeyFromBytes(raw)
			k.secret = [32]byte(raw)
			k.pub = *pub
			secretSet = true
		case len(fields) == 2 && fields[0] == "public":
			if publicLine != "" {
				return nil, fmt.Errorf("%w: duplicate public line", ErrWrongFormat)
			}
			publicLine = fields[1]
		case len(fields) == 0:
			continue
		default:
			return nil, fmt.Errorf("%w: unexpected line %q (want \"secret <hex>\" or \"public <hex>\")", ErrWrongFormat, fields[0])
		}
	}
	if !secretSet {
		return nil, fmt.Errorf("%w: no secret line", ErrWrongFormat)
	}
	if publicLine != "" && publicLine != k.PublicKeyHex() {
		return nil, fmt.Errorf("public line %q does not match derived pubkey %q", publicLine, k.PublicKeyHex())
	}
	return k, nil
}

// Mint writes a fresh random body keypair to dir as body.key (0600) and
// body.pub (0644). Dir is created 0700 if absent. An existing body.key is
// never overwritten — one keypair per shared session, minted once.
func Mint(dir string) (*BodyKey, error) {
	keyPath := filepath.Join(dir, "body.key")
	if err := lstatKeyLeaf(keyPath); err != nil {
		return nil, err // symlinked leaf: confinement refusal, not mint-once
	}
	if _, err := os.Stat(keyPath); err == nil {
		return nil, fmt.Errorf("body key already exists at %s: mint once per shared session, use renew for the tag", keyPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sk, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	k := &BodyKey{pub: *sk.PubKey()}
	raw := sk.Serialize()
	copy(k.secret[:], raw)
	if werr := writeSecretFile(keyPath, k.secret[:]); werr != nil {
		return nil, werr
	}
	pubPath := filepath.Join(dir, "body.pub")
	if err := os.WriteFile(pubPath, []byte(k.PublicKeyHex()+"\n"), 0o644); err != nil {
		return nil, err
	}
	return k, nil
}

// validateScalar enforces scalar ∈ [1, N−1] on the raw 32-byte value.
// PrivKeyFromBytes reduces mod N, so both 0 and ≥ N would otherwise be
// silently accepted (0 maps to the identity-equivalent invalid key; ≥ N
// aliases to a different key than the file's bytes claim).
func validateScalar(raw []byte) error {
	if allZero(raw) {
		return errors.New("secret scalar is zero")
	}
	if new(big.Int).SetBytes(raw).Cmp(curveOrder) >= 0 {
		return errors.New("secret scalar >= curve order")
	}
	return nil
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func writeSecretFile(path string, secret []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()
	if _, err := fmt.Fprintf(f, "secret %s\n", hex.EncodeToString(secret)); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// LoadOrMint returns the session's body key, minting it only if absent.
func LoadOrMint(dir string) (*BodyKey, error) {
	if k, err := Load(filepath.Join(dir, "body.key")); err == nil {
		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrWrongFormat) {
		return nil, err
	}
	return Mint(dir)
}

// AuthTag is a parsed NIP-OA `auth` tag: the four-element
// ["auth", owner-pubkey, conditions, sig] array, with the conditions string
// preserved verbatim (clause order is part of the signed preimage).
type AuthTag struct {
	OwnerPubKey string // 64-char lowercase hex
	Conditions  string // verbatim; never normalized
	Sig         [64]byte
}

// Preimage returns the exact signed message bytes: SHA256 over
// "nostr:agent-auth:" || body-pubkey || ":" || conditions.
func (t AuthTag) Preimage(bodyPubKeyHex string) []byte {
	preimage := AuthPreimagePrefix + bodyPubKeyHex + ":" + t.Conditions
	sum := sha256.Sum256([]byte(preimage))
	return sum[:]
}

// SignAuthTag builds an AuthTag for the given body pubkey by signing
// SHA256(preimage) with the owner secret. Conditions are NOT re-parsed here
// for convenience — ParseConditions validates any string before it becomes
// part of a tag, and mintShareConditions produces the canonical share tag.
func SignAuthTag(ownerSecret [32]byte, bodyPubKeyHex, conditions string) (*AuthTag, error) {
	if err := validateScalar(ownerSecret[:]); err != nil {
		return nil, err
	}
	if len(bodyPubKeyHex) != hexPubkeyLen {
		return nil, fmt.Errorf("body pubkey must be %d hex chars", hexPubkeyLen)
	}
	if _, err := ParseConditions(conditions); err != nil {
		return nil, err
	}
	_, pub := btcec.PrivKeyFromBytes(ownerSecret[:])
	t := &AuthTag{OwnerPubKey: hex.EncodeToString(schnorr.SerializePubKey(pub)), Conditions: conditions}
	priv, _ := btcec.PrivKeyFromBytes(ownerSecret[:])
	sig, err := schnorr.Sign(priv, t.Preimage(bodyPubKeyHex), schnorr.FastSign())
	if err != nil {
		return nil, err
	}
	copy(t.Sig[:], sig.Serialize())
	return t, nil
}

// parseSigHex decodes a 128-char lowercase hex BIP340 signature with the
// same canonical-encoding rules as parsePubKeyHex.
func parseSigHex(s string) ([]byte, error) {
	if len(s) != hexSigLen {
		return nil, fmt.Errorf("sig must be %d hex chars", hexSigLen)
	}
	if strings.ToLower(s) != s {
		return nil, errors.New("sig hex must be lowercase")
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("sig is not hex: %v", err)
	}
	return raw, nil
}

// parsePubKeyHex decodes a 64-char hex pubkey, rejecting non-canonical
// encodings: wrong length, non-hex, or uppercase (the NIP fixes lowercase
// hex; an uppercase owner string would pass a text inequality check while
// decoding to the same key — the self-attestation bypass codex flagged).
func parsePubKeyHex(s string) ([]byte, error) {
	if len(s) != hexPubkeyLen {
		return nil, fmt.Errorf("pubkey must be %d hex chars", hexPubkeyLen)
	}
	if strings.ToLower(s) != s {
		return nil, errors.New("pubkey hex must be lowercase")
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("pubkey is not hex: %v", err)
	}
	return raw, nil
}

// Verify checks the tag's owner signature over the preimage for the given
// body pubkey, that the owner key differs from the body key (decoded
// identity, not text — self-attestation is invalid per the NIP), and that
// the conditions string is grammatically valid (a valid signature over
// malformed conditions must never be storable).
func (t AuthTag) Verify(bodyPubKeyHex string) error {
	if _, err := ParseConditions(t.Conditions); err != nil {
		return fmt.Errorf("conditions invalid: %v", err)
	}
	ownerRaw, err := parsePubKeyHex(t.OwnerPubKey)
	if err != nil {
		return err
	}
	bodyRaw, err := parsePubKeyHex(bodyPubKeyHex)
	if err != nil {
		return err
	}
	if string(ownerRaw) == string(bodyRaw) {
		return errors.New("self-attestation invalid: owner pubkey equals body pubkey")
	}
	owner, err := schnorr.ParsePubKey(ownerRaw)
	if err != nil {
		return fmt.Errorf("owner pubkey is not a valid BIP340 x-only key: %v", err)
	}
	sig, err := schnorr.ParseSignature(t.Sig[:])
	if err != nil {
		return fmt.Errorf("owner signature is not a valid BIP340 signature: %v", err)
	}
	if !sig.Verify(t.Preimage(bodyPubKeyHex), owner) {
		return errors.New("owner signature invalid for preimage")
	}
	return nil
}

// ParseAuthTag parses the four-element tag array. Owner pubkey and sig
// must be lowercase hex (canonical NIP-OA encoding).
func ParseAuthTag(tag []string) (*AuthTag, error) {
	if len(tag) != 4 {
		return nil, fmt.Errorf("auth tag must have exactly 4 elements, has %d", len(tag))
	}
	if tag[0] != "auth" {
		return nil, fmt.Errorf("tag name %q is not \"auth\"", tag[0])
	}
	if _, err := parsePubKeyHex(tag[1]); err != nil {
		return nil, fmt.Errorf("auth tag owner pubkey: %v", err)
	}
	sig, err := parseSigHex(tag[3])
	if err != nil {
		return nil, fmt.Errorf("auth tag sig: %v", err)
	}
	t := &AuthTag{OwnerPubKey: tag[1], Conditions: tag[2]}
	copy(t.Sig[:], sig)
	return t, nil
}

// ShareKinds lists the event kinds a shared-session body may publish; the
// owner signs one NIP-OA condition string per kind (architect ruling
// 2026-09-22: NIP-OA condition strings are conjunctive — every clause must
// hold and `kind=<n>` holds iff event.kind = n — so a multi-kind string is
// satisfiable by no event. One string per kind, all under the one body
// keypair; each event carries the tag naming its own kind).
var ShareKinds = []uint16{20003, 1059, 24200, 44200, 10100}

// ShareConditions is the canonical per-kind conditions string for a share:
// the body may publish exactly kind within the validity window.
func ShareConditions(kind uint16, notAfter int64) string {
	return fmt.Sprintf("kind=%d&created_at<%d", kind, notAfter)
}

// Condition is one parsed clause.
type Condition struct {
	Kind      *uint16
	CreatedLt *uint32
	CreatedGt *uint32
}

// ParseConditions strictly parses a conditions string per the NIP: empty, or
// `clause&clause&...` with only `kind=<n>`, `created_at<t>`, `created_at>t`
// clauses, canonical base-10 (no leading zeros), no whitespace, no empty
// clauses, no trailing/leading '&', no '&&'.
func ParseConditions(raw string) ([]Condition, error) {
	if raw == "" {
		return nil, nil
	}
	if strings.ContainsAny(raw, " \t\n\r") {
		return nil, errors.New("conditions contain whitespace")
	}
	parts := strings.Split(raw, "&")
	out := make([]Condition, 0, len(parts))
	for _, p := range parts {
		c, err := parseClause(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func parseClause(p string) (Condition, error) {
	switch {
	case strings.HasPrefix(p, "kind="):
		v, err := parseCanonicalUint(p[len("kind="):], maxKindValue)
		if err != nil {
			return Condition{}, fmt.Errorf("kind clause %q: %v", p, err)
		}
		k := uint16(v)
		return Condition{Kind: &k}, nil
	case strings.HasPrefix(p, "created_at<"):
		v, err := parseCanonicalUint(p[len("created_at<"):], maxCreatedAtValue)
		if err != nil {
			return Condition{}, fmt.Errorf("created_at< clause %q: %v", p, err)
		}
		lt := uint32(v)
		return Condition{CreatedLt: &lt}, nil
	case strings.HasPrefix(p, "created_at>"):
		v, err := parseCanonicalUint(p[len("created_at>"):], maxCreatedAtValue)
		if err != nil {
			return Condition{}, fmt.Errorf("created_at> clause %q: %v", p, err)
		}
		gt := uint32(v)
		return Condition{CreatedGt: &gt}, nil
	default:
		return Condition{}, fmt.Errorf("unsupported clause %q", p)
	}
}

// parseCanonicalUint enforces canonical base-10: no leading zeros (except
// "0" itself), digits only, within max.
func parseCanonicalUint(s string, max uint64) (uint64, error) {
	if s == "" {
		return 0, errors.New("empty value")
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, errors.New("leading zero")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("non-digit character")
		}
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return 0, errors.New("not a decimal number")
	}
	if !v.IsUint64() || v.Uint64() > max {
		return 0, fmt.Errorf("value out of range (max %d)", max)
	}
	return v.Uint64(), nil
}

// Satisfies evaluates the tag's conditions against an event's kind and
// created_at. Every clause must hold: `kind=<n>` holds iff event.kind = n,
// `created_at<t>` iff event.created_at < t; any false clause rejects, and
// unknown clauses are unsupported (parseClause). Beyond NIP-OA's minimum,
// a credential with NO kind clause is rejected outright (least-privilege:
// a tag must name the kind it authorizes), so a tag authorizes exactly one
// kind — see ShareConditions.
func (t AuthTag) Satisfies(kind uint16, createdAt int64) error {
	conds, err := ParseConditions(t.Conditions)
	if err != nil {
		return err
	}
	sawKind := false
	for _, c := range conds {
		switch {
		case c.Kind != nil:
			sawKind = true
			if *c.Kind != kind {
				return fmt.Errorf("kind %d not authorized (clause kind=%d)", kind, *c.Kind)
			}
		case c.CreatedLt != nil:
			if createdAt >= int64(*c.CreatedLt) {
				return fmt.Errorf("created_at %d not < %d", createdAt, *c.CreatedLt)
			}
		case c.CreatedGt != nil:
			if createdAt <= int64(*c.CreatedGt) {
				return fmt.Errorf("created_at %d not > %d", createdAt, *c.CreatedGt)
			}
		}
	}
	if !sawKind {
		return errors.New("conditions carry no kind clause: a credential must name its kind")
	}
	return nil
}

// PreimageHex returns the lowercase hex of the SHA256 preimage digest for
// this body pubkey and the given conditions string. This is what `share`
// prints for the owner to sign offline.
func (k *BodyKey) PreimageHex(conditions string) string {
	t := AuthTag{Conditions: conditions}
	return hex.EncodeToString(t.Preimage(k.PublicKeyHex()))
}

// SetSigHex decodes a 128-char signature into the tag with the same
// canonical rules as ParseAuthTag (lowercase hex — codex P2: one canonical
// parser for signature text, so anything persisted is accepted by the wire
// parser).
func (t *AuthTag) SetSigHex(s string) error {
	raw, err := parseSigHex(s)
	if err != nil {
		return err
	}
	copy(t.Sig[:], raw)
	return nil
}

// SigHex returns the lowercase hex of the tag signature.
func (t AuthTag) SigHex() string { return hex.EncodeToString(t.Sig[:]) }
