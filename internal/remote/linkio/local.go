package linkio

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// A binding whose consent is local runs a signed task only after the user
// confirms it on this machine (design §7 change 8): amq-remote serves a page
// on localhost that shows the exact text, and the user's local passkey
// (relying party "localhost") signs a challenge bound to the digest shown.
// An agent can run commands as the user; it cannot press Touch ID.

// LocalRPID is the relying party of the local confirmation passkey.
const LocalRPID = "localhost"

// ConfirmPathPrefix is the URL path prefix the local pages are served under.
// The browser extension of the linked server denies exactly this prefix.
const ConfirmPathPrefix = "/amq-remote/confirm/"

const (
	localKeyFile = "local_key.json"
	maxHeld      = 16
)

// LocalOrigin reports whether origin is a loopback page any port may serve:
// http://localhost, http://127.0.0.1 or http://[::1], with a port.
func LocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Path != "" || u.Port() == "" {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// ConfirmChallenge is what the local passkey signs to confirm one held task:
// SHA-256 of "amq.remote.link/1\0confirm\0" and the digest shown.
func ConfirmChallenge(digest string) []byte {
	sum := sha256.Sum256([]byte("amq.remote.link/1\x00confirm\x00" + digest))
	return sum[:]
}

// WriteLocalKey stores a link's local confirmation passkey (public key only).
func WriteLocalKey(stateDir, name string, k ConsentKey) error {
	return writeJSON(LinkDir(stateDir, name), localKeyFile, k)
}

// LoadLocalKey reads a link's local confirmation passkey.
func LoadLocalKey(stateDir, name string) (ConsentKey, error) {
	var k ConsentKey
	err := readJSON(filepath.Join(LinkDir(stateDir, name), localKeyFile), &k)
	return k, err
}

// HasLocalKey reports whether a link has a local confirmation passkey.
func HasLocalKey(stateDir, name string) bool {
	_, err := os.Stat(filepath.Join(LinkDir(stateDir, name), localKeyFile))
	return err == nil
}

// VerifyLocalRegistration checks a WebAuthn registration (navigator.
// credentials.create) for the local passkey: type webauthn.create, the
// challenge the page was given, a loopback origin, the localhost relying
// party, UP and UV, and an attested ES256 (P-256) or Ed25519 key. Attestation
// is "none": what counts is that the user, at this machine, created it.
func VerifyLocalRegistration(challenge []byte, clientDataJSON, attestationObject []byte) (ConsentKey, error) {
	var cd struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if err := json.Unmarshal(clientDataJSON, &cd); err != nil {
		return ConsentKey{}, errors.New("clientDataJSON does not decode")
	}
	switch {
	case cd.Type != "webauthn.create":
		return ConsentKey{}, errors.New("not a webauthn.create")
	case cd.Challenge != b64.EncodeToString(challenge):
		return ConsentKey{}, errors.New("the registration answers another challenge")
	case !LocalOrigin(cd.Origin) || cd.CrossOrigin:
		return ConsentKey{}, errors.New("the registration did not come from a local page")
	}
	att, err := cborDecode(attestationObject)
	if err != nil {
		return ConsentKey{}, fmt.Errorf("attestationObject: %w", err)
	}
	m, ok := att.(map[any]any)
	if !ok {
		return ConsentKey{}, errors.New("attestationObject is not a map")
	}
	authData, ok := m["authData"].([]byte)
	rpHash := sha256.Sum256([]byte(LocalRPID))
	if !ok || len(authData) < 55 || string(authData[:32]) != string(rpHash[:]) {
		return ConsentKey{}, errors.New("the key is not for the localhost relying party")
	}
	flags := authData[32]
	if flags&webauthnFlagUP == 0 || flags&webauthnFlagUV == 0 || flags&0x40 == 0 {
		return ConsentKey{}, errors.New("the user was not verified, or no key was attested")
	}
	// authData: rpIdHash(32) flags(1) counter(4) aaguid(16) idLen(2) id cosePublicKey
	idLen := int(binary.BigEndian.Uint16(authData[53:55]))
	if len(authData) < 55+idLen+1 {
		return ConsentKey{}, errors.New("truncated attested credential data")
	}
	credID := authData[55 : 55+idLen]
	cose, err := cborDecode(authData[55+idLen:])
	if err != nil {
		return ConsentKey{}, fmt.Errorf("credential public key: %w", err)
	}
	spki, alg, err := coseToSPKI(cose)
	if err != nil {
		return ConsentKey{}, err
	}
	return ConsentKey{
		CredentialID: b64.EncodeToString(credID), SPKI: b64.EncodeToString(spki), Alg: alg,
		RPID: LocalRPID, BackupEligible: flags&webauthnFlagBE != 0,
	}, nil
}

// VerifyLocalConfirm checks the local passkey's assertion over the confirm
// challenge of digest, made on a loopback page.
func VerifyLocalConfirm(key ConsentKey, digest, authenticatorData, clientDataJSON, signature string) error {
	return verifyWebAuthnGet(key, ConfirmChallenge(digest), LocalOrigin, authenticatorData, clientDataJSON, signature)
}

// coseToSPKI converts a COSE_Key (EC2 P-256 / ES256, or OKP Ed25519) to SPKI DER.
func coseToSPKI(v any) ([]byte, int, error) {
	m, ok := v.(map[any]any)
	if !ok {
		return nil, 0, errors.New("the credential public key is not a COSE map")
	}
	switch kty, alg := m[int64(1)], m[int64(3)]; {
	case kty == int64(2) && alg == int64(-7) && m[int64(-1)] == int64(1):
		x, okx := m[int64(-2)].([]byte)
		y, oky := m[int64(-3)].([]byte)
		if !okx || !oky || len(x) != 32 || len(y) != 32 {
			return nil, 0, errors.New("malformed P-256 key")
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return nil, 0, errors.New("the P-256 point is not on the curve")
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		return der, -7, err
	case kty == int64(1) && alg == int64(-8) && m[int64(-1)] == int64(6):
		x, okx := m[int64(-2)].([]byte)
		if !okx || len(x) != ed25519.PublicKeySize {
			return nil, 0, errors.New("malformed Ed25519 key")
		}
		der, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(x))
		return der, -8, err
	}
	return nil, 0, errors.New("the passkey uses an algorithm other than ES256 or Ed25519")
}

// HeldTask is a signed task on a local binding, waiting for the user's
// confirmation. It is immutable and lives only in this process: a restart
// loses it, and the next hello no longer lists its digest.
type HeldTask struct {
	ID         string    `json:"id"` // the first 8 hex of the request id
	Digest     string    `json:"digest"`
	Binding    string    `json:"binding"`
	Session    string    `json:"session"` // the binding's label
	Text       string    `json:"text"`
	NotAfter   time.Time `json:"not_after"`
	verified   *Verified
	credential string
}

// held is the bounded set of tasks waiting for a local confirmation, keyed by
// digest, outside the store's executable states.
type held struct {
	mu    sync.Mutex
	tasks map[string]*HeldTask
}

func (h *held) add(t *HeldTask, now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked(now)
	if _, dup := h.tasks[t.Digest]; dup {
		return nil
	}
	if len(h.tasks) >= maxHeld {
		return refuse(string(protocol.CodeBusy), "%d tasks already wait for a local confirmation", maxHeld)
	}
	h.tasks[t.Digest] = t
	return nil
}

func (h *held) expireLocked(now time.Time) {
	for d, t := range h.tasks {
		if !now.Before(t.NotAfter) {
			delete(h.tasks, d)
		}
	}
}

// list returns the held tasks, oldest deadline first.
func (h *held) list(now time.Time) []HeldTask {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked(now)
	out := make([]HeldTask, 0, len(h.tasks))
	for _, t := range h.tasks {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NotAfter.Before(out[j].NotAfter) })
	return out
}

// digests lists the held digests for hello.
func (h *held) digests(now time.Time) []string {
	out := []string{}
	for _, t := range h.list(now) {
		out = append(out, t.Digest)
	}
	return out
}

// take removes and returns the held task with that id and digest, so a
// confirmation moves it into the store at most once.
func (h *held) take(id, digest string, now time.Time) (*HeldTask, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked(now)
	t := h.tasks[digest]
	if t == nil || t.ID != id {
		return nil, refuse(string(protocol.CodeNotFound), "no task %s waits for confirmation with that text; it may have expired", id)
	}
	delete(h.tasks, digest)
	return t, nil
}

// heldID is the short id a person types: the first 8 hex of the request id.
func heldID(requestID string) string {
	return strings.ReplaceAll(requestID, "-", "")[:8]
}
