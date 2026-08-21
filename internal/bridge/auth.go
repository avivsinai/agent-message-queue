package bridge

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
	generation = strings.TrimSpace(generation)
	if generation == "" {
		return HostKey{}, fmt.Errorf("key generation is required")
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return HostKey{}, fmt.Errorf("generate host key: %w", err)
	}
	return HostKey{Generation: generation, Private: priv}, nil
}

func CanonicalBytes(env Envelope) []byte {
	return []byte(strings.Join([]string{
		"amq-bridge-envelope-v1",
		"version=" + strconv.Itoa(env.Version),
		"transfer_id=" + env.TransferID,
		"source_host=" + env.SourceHost,
		"source_handle=" + env.SourceHandle,
		"dest_alias=" + env.DestAlias,
		"source_message_id=" + env.SourceMessageID,
		"thread_id=" + env.ThreadID,
		"payload_sha256=" + strings.ToLower(env.PayloadSHA256),
		"key_generation=" + env.KeyGeneration,
	}, "\n"))
}

func SignEnvelope(env *Envelope, key HostKey) error {
	if env == nil {
		return fmt.Errorf("bridge envelope is required")
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
	host := strings.TrimSpace(string(data))
	if err := fsq.ValidateHandle(host); err != nil {
		return "", fmt.Errorf("bridge host-id: %w", err)
	}
	if string(data) != host+"\n" && string(data) != host {
		return "", fmt.Errorf("bridge host-id %s has surrounding whitespace", path)
	}
	return host, nil
}

func WriteHostID(root, host string) error {
	if err := fsq.ValidateHandle(host); err != nil {
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
	if err := os.MkdirAll(filepath.Join(root, "bridge"), 0o700); err != nil {
		return fmt.Errorf("create bridge directory: %w", err)
	}
	body := fmt.Sprintf("generation %s\nseed %s\n", key.Generation, hex.EncodeToString(key.Private.Seed()))
	return writePrivateFile(IdentityPath(root), []byte(body))
}

func LoadTrusted(root, host string) (ed25519.PublicKey, string, error) {
	if err := fsq.ValidateHandle(host); err != nil {
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

func WriteTrusted(root, host string, pub ed25519.PublicKey, generation string) error {
	if err := fsq.ValidateHandle(host); err != nil {
		return fmt.Errorf("trusted source host: %w", err)
	}
	generation = strings.TrimSpace(generation)
	if generation == "" {
		return fmt.Errorf("trusted host generation is required")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("trusted host public key must be %d bytes", ed25519.PublicKeySize)
	}
	if err := os.MkdirAll(filepath.Join(root, "bridge", TrustedDirName), 0o700); err != nil {
		return fmt.Errorf("create trusted directory: %w", err)
	}
	body := fmt.Sprintf("generation %s\npublic %s\n", generation, hex.EncodeToString(pub))
	return writePrivateFile(TrustedPath(root, host), []byte(body))
}

func readKeyFields(path, secretField string) (map[string]string, error) {
	data, err := readPrivateKeyFile(path)
	if err != nil {
		return nil, err
	}
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
	if generation == "" || generation != strings.TrimSpace(generation) {
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

func readPrivateKeyFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return nil, fmt.Errorf("%s mode is %o, want 0600", path, got)
	}
	return fsq.ReadRegularNoFollow(path)
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
