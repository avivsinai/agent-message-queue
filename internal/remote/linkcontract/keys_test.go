package linkcontract

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
)

// The test keys are derived from public seed strings, so no private key is
// committed and any implementation can rebuild them (testdata/link/README.md).
const (
	consentKeySeed   = "amq.remote.link/1 test consent key"
	deviceKeySeed    = "amq.remote.link/1 test device key"
	credentialIDSeed = "amq.remote.link/1 test credential"
	// The Ed25519 (alg -8) consent passkey.
	consentKeyEdSeed   = "amq.remote.link/1 test consent key ed25519"
	credentialIDEdSeed = "amq.remote.link/1 test credential ed25519"
)

// Fixed values of the golden exchange. None names a real server.
const (
	serverID     = "srv_example"
	rpID         = "sign.example.test"
	origin       = "https://" + rpID
	storeID      = "st_5d1e0c2a"
	bindingName  = "pi-demo"
	targetID     = "pi:demo-5b9d0c1e"
	epoch        = "e_4c0e1a"
	nativeID     = "pi:3f9c2b7a"
	requestID    = "7f3a2c1e-5b9d-4e8a-9c21-7d4e5f6a8b90"
	issuedAt     = "2026-10-09T14:02:00Z"
	notAfter     = "2026-10-09T14:04:00Z"
	taskText     = "Rebase fix/ingest on main & run the unit tests <fast>.\nReport the result."
	helloNonceID = "amq.remote.link/1 test nonce"
)

var b64 = base64.RawURLEncoding

func seed(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// consentKey is the software authenticator's ES256 key: the scalar is
// SHA-256(seed) mod (n-1) + 1, so it is always a valid P-256 private key.
func consentKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	n := new(big.Int).Sub(elliptic.P256().Params().N, big.NewInt(1))
	d := new(big.Int).Mod(new(big.Int).SetBytes(seed(consentKeySeed)), n)
	d.Add(d, big.NewInt(1))
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d.FillBytes(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// deviceKey is the endpoint's Ed25519 device key: seed = SHA-256(seed string).
func deviceKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(seed(deviceKeySeed))
}

func credentialID() string { return b64.EncodeToString(seed(credentialIDSeed)[:16]) }

// consentKeyEd25519 is the Ed25519 consent passkey: seed = SHA-256(seed string).
func consentKeyEd25519() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(seed(consentKeyEdSeed)) }

func credentialIDEd25519() string { return b64.EncodeToString(seed(credentialIDEdSeed)[:16]) }

func helloNonce() string { return b64.EncodeToString(seed(helloNonceID)) }

func spki(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// creatorHost names the endpoint as a sink: "link-" and the first 16 hex
// digits of SHA-256(device key SPKI DER).
func creatorHost(deviceSPKI []byte) string {
	sum := sha256.Sum256(deviceSPKI)
	return "link-" + hex.EncodeToString(sum[:8])
}

// fingerprint is the design's 100-bit passkey fingerprint: base32 (RFC 4648,
// no padding) of SHA-256(JCS({credential_id, spki, alg, rp_id, origin})),
// first 20 characters, shown in five groups of four.
func fingerprint(t *testing.T, credID string, keySPKI []byte, alg int, rp, org string) string {
	t.Helper()
	doc, err := jcs.Marshal(map[string]any{
		"credential_id": credID, "spki": b64.EncodeToString(keySPKI), "alg": alg, "rp_id": rp, "origin": org,
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(doc)
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])[:20]
	groups := make([]string, 0, 5)
	for i := 0; i < 20; i += 4 {
		groups = append(groups, s[i:i+4])
	}
	return strings.Join(groups, "-")
}

// helloMessage is the byte string the device key signs in hello.
func helloMessage(server, nonce, store string) []byte {
	return []byte("amq.remote.link/1\x00hello\x00" + server + "\x00" + nonce + "\x00" + store)
}
