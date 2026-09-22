package bridge

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	IdentityFileName = "identity"
	HostIDFileName   = "host-id"
	TrustedDirName   = "trusted"
)

// HostKey is one Ed25519 generation for a bridge host principal.
type HostKey struct {
	Generation string
	Private    ed25519.PrivateKey
}

func (k HostKey) Public() ed25519.PublicKey {
	return k.Private.Public().(ed25519.PublicKey)
}

func IdentityPath(root string) string {
	return filepath.Join(root, "bridge", IdentityFileName)
}

func HostIDPath(root string) string {
	return filepath.Join(root, "bridge", HostIDFileName)
}

func TrustedPath(root, host string) string {
	return filepath.Join(root, "bridge", TrustedDirName, host)
}

func GenerateHostKey(generation string) (HostKey, error) {
	if err := validateBridgeIdentifier("key generation", generation); err != nil {
		return HostKey{}, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return HostKey{}, fmt.Errorf("generate host key: %w", err)
	}
	return HostKey{Generation: generation, Private: priv}, nil
}

func CanonicalBytes(env Envelope) []byte {
	var preimage bytes.Buffer
	appendField := func(value string) {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = preimage.Write(length[:])
		_, _ = preimage.WriteString(value)
	}
	appendField("amq-bridge-envelope-v2")
	appendField("2")
	appendField(env.TransferID)
	appendField(env.SourceHost)
	appendField(env.SourceHandle)
	appendField(env.DestAlias)
	appendField(env.SourceMessageID)
	appendField(env.ThreadID)
	appendField(env.PayloadSHA256)
	appendField(env.KeyGeneration)
	return preimage.Bytes()
}

func SignEnvelope(env *Envelope, key HostKey) error {
	if env == nil {
		return fmt.Errorf("bridge envelope is required")
	}
	if err := validateEnvelope(*env, false); err != nil {
		return err
	}
	if env.KeyGeneration != key.Generation {
		return fmt.Errorf("envelope key_generation %q does not match identity generation %q", env.KeyGeneration, key.Generation)
	}
	env.Signature = hex.EncodeToString(ed25519.Sign(key.Private, CanonicalBytes(*env)))
	return ValidateEnvelope(*env)
}

func VerifyEnvelope(env Envelope, pub ed25519.PublicKey, generation string) error {
	if err := ValidateEnvelope(env); err != nil {
		return err
	}
	if err := validateBridgeIdentifier("trusted key generation", generation); err != nil {
		return err
	}
	if env.KeyGeneration != generation {
		return fmt.Errorf("envelope key_generation %q is not the trusted generation %q", env.KeyGeneration, generation)
	}
	sig, err := hex.DecodeString(env.Signature)
	if err != nil {
		return fmt.Errorf("bridge envelope signature: %w", err)
	}
	if !ed25519.Verify(pub, CanonicalBytes(env), sig) {
		return fmt.Errorf("bridge envelope signature does not match source host %q", env.SourceHost)
	}
	return nil
}

func LoadHostID(root string) (string, error) {
	path := HostIDPath(root)
	data, err := readPrivateKeyFile(path)
	if err != nil {
		return "", fmt.Errorf("bridge host-id: %w", err)
	}
	return parseHostID(path, data)
}

// LoadHostIDFromDeliveryRoot reads host-id through an already-authorized
// delivery-root capability so the identity used for routing cannot be
// replaced through the ambient root path after authorization.
func LoadHostIDFromDeliveryRoot(root *fsq.DeliveryRoot) (string, error) {
	const rel = "bridge/host-id"
	data, err := readPrivateKeyRootFile(root, rel)
	if err != nil {
		return "", fmt.Errorf("bridge host-id: %w", err)
	}
	return parseHostID(root.DisplayPath(rel), data)
}

func parseHostID(path string, data []byte) (string, error) {
	host := strings.TrimSpace(string(data))
	if err := validateBridgeIdentifier("host-id", host); err != nil {
		return "", fmt.Errorf("bridge host-id: %w", err)
	}
	if string(data) != host+"\n" && string(data) != host {
		return "", fmt.Errorf("bridge host-id %s has surrounding whitespace", path)
	}
	return host, nil
}

func WriteHostID(root, host string) error {
	if err := validateBridgeIdentifier("host-id", host); err != nil {
		return fmt.Errorf("bridge host-id: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bridge"), 0o700); err != nil {
		return fmt.Errorf("create bridge directory: %w", err)
	}
	return writePrivateFile(HostIDPath(root), []byte(host+"\n"))
}

func LoadIdentity(root string) (HostKey, error) {
	fields, err := readKeyFields(IdentityPath(root), "seed")
	if err != nil {
		return HostKey{}, fmt.Errorf("bridge identity: %w", err)
	}
	seed, err := hex.DecodeString(fields["seed"])
	if err != nil {
		return HostKey{}, fmt.Errorf("bridge identity seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return HostKey{}, fmt.Errorf("bridge identity seed must be %d bytes", ed25519.SeedSize)
	}
	return HostKey{Generation: fields["generation"], Private: ed25519.NewKeyFromSeed(seed)}, nil
}

func WriteIdentity(root string, key HostKey) error {
	if err := validateBridgeIdentifier("key generation", key.Generation); err != nil {
		return err
	}
	if len(key.Private) != ed25519.PrivateKeySize {
		return fmt.Errorf("bridge identity private key must be %d bytes", ed25519.PrivateKeySize)
	}
	if err := os.MkdirAll(filepath.Join(root, "bridge"), 0o700); err != nil {
		return fmt.Errorf("create bridge directory: %w", err)
	}
	body := fmt.Sprintf("generation %s\nseed %s\n", key.Generation, hex.EncodeToString(key.Private.Seed()))
	return writePrivateFile(IdentityPath(root), []byte(body))
}

func LoadTrusted(root, host string) (ed25519.PublicKey, string, error) {
	if err := validateBridgeIdentifier("trusted source host", host); err != nil {
		return nil, "", fmt.Errorf("trusted source host: %w", err)
	}
	fields, err := readKeyFields(TrustedPath(root, host), "public")
	if err != nil {
		return nil, "", fmt.Errorf("trusted host %s: %w", host, err)
	}
	pub, err := hex.DecodeString(fields["public"])
	if err != nil {
		return nil, "", fmt.Errorf("trusted host %s public key: %w", host, err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, "", fmt.Errorf("trusted host %s public key must be %d bytes", host, ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(pub), fields["generation"], nil
}

// LoadTrustedFromDeliveryRoot reads a trusted public key through an
// already-authorized delivery-root capability. The source host remains an
// envelope claim until this exact trusted file is loaded and signature
// verification succeeds.
func LoadTrustedFromDeliveryRoot(root *fsq.DeliveryRoot, host string) (ed25519.PublicKey, string, error) {
	if err := validateBridgeIdentifier("trusted source host", host); err != nil {
		return nil, "", fmt.Errorf("trusted source host: %w", err)
	}
	rel := filepath.Join("bridge", TrustedDirName, host)
	data, err := readPrivateKeyRootFile(root, rel)
	if err != nil {
		return nil, "", fmt.Errorf("trusted host %s: %w", host, err)
	}
	fields, err := parseKeyFields(root.DisplayPath(rel), data, "public")
	if err != nil {
		return nil, "", fmt.Errorf("trusted host %s: %w", host, err)
	}
	pub, err := hex.DecodeString(fields["public"])
	if err != nil {
		return nil, "", fmt.Errorf("trusted host %s public key: %w", host, err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, "", fmt.Errorf("trusted host %s public key must be %d bytes", host, ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(pub), fields["generation"], nil
}

// WriteTrusted provisions (or, with ReplaceTrusted, rotates) a source
// host's trusted key record. The create-new path never overwrites: a
// rotation must go through ReplaceTrusted, which refuses a generation
// downgrade and lands the new body via tmp+rename.
func WriteTrusted(root, host string, pub ed25519.PublicKey, generation string) error {
	return writeTrusted(root, host, pub, generation, false)
}

// ReplaceTrusted atomically overwrites a source host's trusted record with
// a NEWER generation (rotation). It refuses a downgrade — an operator who
// re-provisions an older key is almost certainly undoing a rotation by
// mistake — and it never leaves a partial file: the new body is written to
// a temp file in the trusted directory and renamed over the target
// (review-845-r1 P1-1: writePrivateFile's O_EXCL made rotation a raw
// "file exists" error with the only remedy a hand-delete).
func ReplaceTrusted(root, host string, pub ed25519.PublicKey, generation string) error {
	return writeTrusted(root, host, pub, generation, true)
}

func writeTrusted(root, host string, pub ed25519.PublicKey, generation string, replace bool) error {
	if err := validateBridgeIdentifier("trusted source host", host); err != nil {
		return fmt.Errorf("trusted source host: %w", err)
	}
	if err := validateBridgeIdentifier("trusted host generation", generation); err != nil {
		return err
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("trusted host public key must be %d bytes", ed25519.PublicKeySize)
	}
	dir := filepath.Join(root, "bridge", TrustedDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create trusted directory: %w", err)
	}
	path := TrustedPath(root, host)
	if !replace {
		err := writePrivateFile(path, []byte(trustedBody(pub, generation)))
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("trusted host %s already provisioned; use --replace to rotate", host)
		}
		return err
	}
	if _, current, lerr := LoadTrusted(root, host); lerr == nil {
		ordered, reason := compareGenerations(current, generation)
		if !ordered {
			return fmt.Errorf("trusted host %s: cannot order generations %q and %q; provision with a generated label", host, current, generation)
		}
		if reason < 0 {
			return fmt.Errorf("trusted host %s: refusing to rotate generation %q back to %q", host, current, generation)
		}
	}
	body := []byte(trustedBody(pub, generation))
	tmp, err := os.CreateTemp(dir, ".trusted-*")
	if err != nil {
		return fmt.Errorf("create trusted temp file: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod trusted temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write trusted temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync trusted temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close trusted temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace trusted host %s: %w", host, err)
	}
	return nil
}

// compareGenerations orders generation labels for the --replace downgrade
// guard (review-845-r2 P1: a byte-string compare inverts across the digit
// boundary — 9 -> 10 refused, 10 -> 9 allowed). Generations are unsigned
// decimal integers as minted by identity init, so all-digit labels compare
// numerically; any other label class (or a mixed pair) has no defined
// order and is reported as unordered rather than guessed at. Returns
// (ordered, cmp) where cmp is -1 when b is older, +1 when b is newer, and
// 0 when the labels are equal.
func compareGenerations(a, b string) (bool, int) {
	if a == b {
		return true, 0
	}
	for _, s := range []string{a, b} {
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return false, 0
			}
		}
	}
	na, aerr := strconv.ParseUint(a, 10, 64)
	nb, berr := strconv.ParseUint(b, 10, 64)
	if aerr != nil || berr != nil {
		return false, 0
	}
	switch {
	case nb < na:
		return true, -1
	case nb > na:
		return true, 1
	default:
		return true, 0
	}
}

func trustedBody(pub ed25519.PublicKey, generation string) string {
	return fmt.Sprintf("generation %s\npublic %s\n", generation, hex.EncodeToString(pub))
}

func readKeyFields(path, secretField string) (map[string]string, error) {
	data, err := readPrivateKeyFile(path)
	if err != nil {
		return nil, err
	}
	return parseKeyFields(path, data, secretField)
}

func parseKeyFields(path string, data []byte, secretField string) (map[string]string, error) {
	fields := map[string]string{}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || key == "" || value == "" || strings.ContainsAny(value, " \t") {
			return nil, fmt.Errorf("%s line %d is invalid", path, i+1)
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("%s repeats field %q", path, key)
		}
		fields[key] = value
	}
	generation := fields["generation"]
	if err := validateBridgeIdentifier("generation", generation); err != nil {
		return nil, fmt.Errorf("%s generation is invalid", path)
	}
	if fields[secretField] == "" {
		return nil, fmt.Errorf("%s missing %s", path, secretField)
	}
	if len(fields) != 2 {
		return nil, fmt.Errorf("%s has unknown fields", path)
	}
	return fields, nil
}

func readPrivateKeyRootFile(root *fsq.DeliveryRoot, rel string) ([]byte, error) {
	if root == nil {
		return nil, fmt.Errorf("delivery root is required")
	}
	file, info, err := root.OpenRegularNoFollow(rel)
	if err != nil {
		return nil, err
	}
	path := root.DisplayPath(rel)
	if err := validatePrivateKeyInfo(path, info); err != nil {
		_ = file.Close()
		return nil, err
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func readPrivateKeyFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateKeyInfo(path, info); err != nil {
		return nil, err
	}
	return fsq.ReadRegularNoFollow(path)
}

func validatePrivateKeyInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("%s mode is %o, want 0600", path, got)
	}
	return nil
}

func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// ParsePublicIdentity decodes a bridge identity's public half from the two
// shapes an operator actually has in hand (bead agent-message-queue-ug3):
// the one line `amq-bridge identity public` prints ("host=<h>
// generation=<g> public=<hex>") or the two-line key-file record the loader
// itself accepts ("generation <g>" + "public <hex>"). The returned host is
// empty for the two-line shape, which carries none. The hex public key
// must decode to an Ed25519 public key. Both shapes are normalised before
// WriteTrusted re-validates host, generation and key size and emits the
// canonical two-line body, so whatever is accepted here round-trips
// through LoadTrusted (review-845-r1 P2-2: the safety property is that
// normalisation, not literal parser sharing).
func ParsePublicIdentity(data []byte) (host, generation string, pub ed25519.PublicKey, err error) {
	fields, perr := parseKeyFields("identity record", data, "public")
	if perr == nil {
		pub, err = decodePublicIdentity(fields)
		return "", fields["generation"], pub, err
	}
	// Shape disambiguation first (review-845-r1 P2-3): the two-line shape
	// contains no '='-keyed fields, so if the input is not one-line-shaped
	// the honest error is the parser's, never a synthesized "line 1 is
	// invalid". Empty input is the no-input case.
	first := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			first = strings.TrimSpace(line)
			break
		}
	}
	if first == "" || !strings.Contains(first, "=") {
		return "", "", nil, perr
	}
	// One-line shape: host=<h> generation=<g> public=<hex>. Unknown fields
	// are refused with the same strictness parseKeyFields applies to the
	// two-line shape (P2-2) — a future protocol field must be an explicit
	// refusal, never a silent drop.
	one := map[string]string{}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, part := range strings.Fields(line) {
			key, value, ok := strings.Cut(part, "=")
			if !ok || key == "" || value == "" {
				return "", "", nil, fmt.Errorf("identity record line %d is invalid", i+1)
			}
			if _, exists := one[key]; exists {
				return "", "", nil, fmt.Errorf("identity record repeats field %q", key)
			}
			one[key] = value
		}
	}
	if _, ok := one["host"]; !ok {
		return "", "", nil, fmt.Errorf("identity record: %w", perr)
	}
	extra := make([]string, 0, len(one))
	for k := range one {
		if k != "host" && k != "generation" && k != "public" {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return "", "", nil, fmt.Errorf("identity record has unknown fields: %s", strings.Join(extra, ", "))
	}
	if err := validateBridgeIdentifier("generation", one["generation"]); err != nil {
		return "", "", nil, fmt.Errorf("identity record generation is invalid")
	}
	pub, err = decodePublicIdentity(map[string]string{"public": one["public"]})
	return one["host"], one["generation"], pub, err
}

func decodePublicIdentity(fields map[string]string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(fields["public"])
	if err != nil {
		return nil, fmt.Errorf("identity record public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity record public key must be %d bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}
