package linkio

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
)

// On-disk state, all under the endpoint's state dir (one AMQ root):
//
//	link/store_id               this store's identity, minted once
//	link/<name>/device.key      the link's Ed25519 device key, 0600
//	link/<name>/link.json       what was pinned at linking
//	link/<name>/consent_keys.json  the consent public keys this machine trusts
//	link/retired/<host>         a retired sink; its records settle without network
const (
	deviceKeyFile   = "device.key"
	linkFile        = "link.json"
	consentKeysFile = "consent_keys.json"
	storeIDFile     = "store_id"
	retiredDir      = "retired"
)

var b64 = base64.RawURLEncoding

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidName reports whether s can name a link.
func ValidName(s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("link name %q: use 1-32 lowercase letters, digits, - or _", s)
	}
	return nil
}

// Dir is the state directory of all links in one root.
func Dir(stateDir string) string { return filepath.Join(stateDir, "link") }

// LinkDir is the state directory of one link.
func LinkDir(stateDir, name string) string { return filepath.Join(Dir(stateDir), name) }

// Pin is what a link fixed at linking. The endpoint refuses a server whose
// challenge names another server_id.
type Pin struct {
	URL        string `json:"url"`
	ServerID   string `json:"server_id"`
	DeviceID   string `json:"device_id"`
	User       string `json:"user"`
	DeviceName string `json:"device_name"`
	RPID       string `json:"rp_id"`
	Origin     string `json:"origin"`
}

// ConsentKey is one consent public key this machine trusts.
type ConsentKey struct {
	CredentialID   string `json:"credential_id"`
	SPKI           string `json:"spki"`
	Alg            int    `json:"alg"`
	RPID           string `json:"rp_id"`
	Origin         string `json:"origin"`
	BackupEligible bool   `json:"backup_eligible"`
}

// DeviceKey is a link's device key and the names derived from it.
type DeviceKey struct {
	Private ed25519.PrivateKey
	SPKI    []byte
}

// Host is the creator host of every record this link creates: "link-" and
// the first 16 hex digits of SHA-256(SPKI). It is also the link's sink.
func (k DeviceKey) Host() string { return HostOf(k.SPKI) }

// HostOf derives the creator host from a device key's SPKI DER.
func HostOf(spki []byte) string {
	sum := sha256.Sum256(spki)
	return "link-" + hex.EncodeToString(sum[:8])
}

// Sign signs msg with the device key.
func (k DeviceKey) Sign(msg []byte) []byte { return ed25519.Sign(k.Private, msg) }

func newDeviceKey(seed []byte) (DeviceKey, error) {
	priv := ed25519.NewKeyFromSeed(seed)
	spki, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return DeviceKey{}, err
	}
	return DeviceKey{Private: priv, SPKI: spki}, nil
}

// MintDeviceKey creates a link's device key. It refuses to overwrite one:
// a new key is a new machine to the server.
func MintDeviceKey(stateDir, name string) (DeviceKey, error) {
	dir := LinkDir(stateDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return DeviceKey{}, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return DeviceKey{}, err
	}
	path := filepath.Join(dir, deviceKeyFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return DeviceKey{}, fmt.Errorf("link %q already has a device key; remove the link first", name)
		}
		return DeviceKey{}, err
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintf(f, "ed25519 %s\n", hex.EncodeToString(seed)); err != nil {
		return DeviceKey{}, err
	}
	if err := f.Sync(); err != nil {
		return DeviceKey{}, err
	}
	return newDeviceKey(seed)
}

// LoadDeviceKey reads a link's device key. A key readable by group or
// others is refused.
func LoadDeviceKey(stateDir, name string) (DeviceKey, error) {
	return loadDeviceKey(stateDir, name, true)
}

// loadDeviceKey reads the key; private refuses a key readable by others,
// which retiring does not need: it only reads the public host.
func loadDeviceKey(stateDir, name string, private bool) (DeviceKey, error) {
	path := filepath.Join(LinkDir(stateDir, name), deviceKeyFile)
	info, err := os.Lstat(path)
	if err != nil {
		return DeviceKey{}, err
	}
	if !info.Mode().IsRegular() {
		return DeviceKey{}, fmt.Errorf("%s is not a regular file", path)
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return DeviceKey{}, fmt.Errorf("%s is readable by others (mode %o); it must be 0600", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DeviceKey{}, err
	}
	hexSeed, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ed25519 ")
	seed, derr := hex.DecodeString(hexSeed)
	if !ok || derr != nil || len(seed) != ed25519.SeedSize {
		return DeviceKey{}, fmt.Errorf("%s is not a device key", path)
	}
	return newDeviceKey(seed)
}

// StoreID returns this root's store id, minting it on first use. A wiped or
// replaced store gets a new id, so a consent signed for the old store can
// never be replayed into the new one.
func StoreID(stateDir string) (string, error) {
	path := filepath.Join(Dir(stateDir), storeIDFile)
	if data, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(data))
		if id == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := "st_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	if err := os.MkdirAll(Dir(stateDir), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return StoreID(stateDir) // another process minted it first
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(id + "\n"); err != nil {
		return "", err
	}
	return id, f.Sync()
}

// WritePin records what a link fixed at linking.
func WritePin(stateDir, name string, p Pin) error {
	return writeJSON(LinkDir(stateDir, name), linkFile, p)
}

// LoadPin reads a link's pin.
func LoadPin(stateDir, name string) (Pin, error) {
	var p Pin
	err := readJSON(filepath.Join(LinkDir(stateDir, name), linkFile), &p)
	return p, err
}

// WriteConsentKeys replaces the consent keys a link trusts.
func WriteConsentKeys(stateDir, name string, keys []ConsentKey) error {
	if keys == nil {
		keys = []ConsentKey{}
	}
	return writeJSON(LinkDir(stateDir, name), consentKeysFile, keys)
}

// LoadConsentKeys reads the consent keys a link trusts.
func LoadConsentKeys(stateDir, name string) ([]ConsentKey, error) {
	var keys []ConsentKey
	err := readJSON(filepath.Join(LinkDir(stateDir, name), consentKeysFile), &keys)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return keys, err
}

// DropConsentKey removes one consent key. Removing a key only lowers
// authority, so the server may ask for it; adding one never comes from the
// server.
func DropConsentKey(stateDir, name, credentialID string) (bool, error) {
	keys, err := LoadConsentKeys(stateDir, name)
	if err != nil {
		return false, err
	}
	kept := keys[:0]
	for _, k := range keys {
		if k.CredentialID != credentialID {
			kept = append(kept, k)
		}
	}
	if len(kept) == len(keys) {
		return false, nil
	}
	return true, WriteConsentKeys(stateDir, name, kept)
}

// Retire makes a link's sink permanent history: it writes the retirement
// for its host, then deletes the link's device key and state, so the link
// can never dial again. Records created by the host settle their
// caller-delivery part without network. A link created again under the same
// name gets a new key, so a new host that never receives the old records.
func Retire(stateDir, name string) (string, error) {
	key, err := loadDeviceKey(stateDir, name, false)
	if err != nil {
		return "", err
	}
	host := key.Host()
	dir := filepath.Join(Dir(stateDir), retiredDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if _, err := fsq.WriteFileAtomic(dir, host, []byte(name+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.RemoveAll(LinkDir(stateDir, name)); err != nil {
		return host, err
	}
	return host, nil
}

// Retired lists the retired sinks of one root.
func Retired(stateDir string) (map[string]bool, error) {
	named, err := RetiredLinks(stateDir)
	out := make(map[string]bool, len(named))
	for host := range named {
		out[host] = true
	}
	return out, err
}

// RetiredLinks maps each retired sink to the link name it had.
func RetiredLinks(stateDir string) (map[string]string, error) {
	dir := filepath.Join(Dir(stateDir), retiredDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "link-") {
			continue
		}
		name, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out[e.Name()] = strings.TrimSpace(string(name))
	}
	return out, nil
}

func writeJSON(dir, file string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(dir, file, append(data, '\n'), 0o600)
	return err
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Fingerprint is the 100-bit passkey fingerprint a person compares between
// the browser that created the key and this machine: base32 (RFC 4648, no
// padding) of SHA-256(JCS({credential_id, spki, alg, rp_id, origin})), the
// first 20 characters, in five groups of four.
func Fingerprint(k ConsentKey) (string, error) {
	doc, err := jcs.Marshal(map[string]any{
		"credential_id": k.CredentialID, "spki": k.SPKI, "alg": k.Alg, "rp_id": k.RPID, "origin": k.Origin,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(doc)
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])[:20]
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20], nil
}

// SameFingerprint compares a typed fingerprint with a computed one, ignoring
// case, spaces and dashes.
func SameFingerprint(typed, want string) bool {
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '-' || r == ' ' {
				return -1
			}
			return r
		}, strings.ToUpper(strings.TrimSpace(s)))
	}
	return norm(typed) != "" && norm(typed) == norm(want)
}

// RedeemMessage is the byte string the device key signs to redeem a code.
func RedeemMessage(serverID, code string, ts int64) []byte {
	return []byte("amq.remote.link/1\x00redeem\x00" + serverID + "\x00" + code + "\x00" + strconv.FormatInt(ts, 10))
}
