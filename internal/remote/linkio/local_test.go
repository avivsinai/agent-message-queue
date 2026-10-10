package linkio

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// localAuthenticator is a software authenticator for the localhost relying
// party: it registers an Ed25519 passkey and signs confirm challenges.
type localAuthenticator struct {
	priv   ed25519.PrivateKey
	credID []byte
}

func newLocalAuthenticator() *localAuthenticator {
	seed := sha256.Sum256([]byte("amq.remote.link/1 test local key"))
	return &localAuthenticator{priv: ed25519.NewKeyFromSeed(seed[:]), credID: []byte("local-cred-1")}
}

// register answers navigator.credentials.create for challenge on origin.
func (a *localAuthenticator) register(challenge []byte, origin string) (clientData, attestation []byte) {
	clientData = []byte(`{"type":"webauthn.create","challenge":"` + b64.EncodeToString(challenge) + `","origin":"` + origin + `","crossOrigin":false}`)
	rp := sha256.Sum256([]byte(LocalRPID))
	auth := append(rp[:], webauthnFlagUP|webauthnFlagUV|0x40)
	auth = binary.BigEndian.AppendUint32(auth, 0)
	auth = append(auth, make([]byte, 16)...) // aaguid
	auth = binary.BigEndian.AppendUint16(auth, uint16(len(a.credID)))
	auth = append(auth, a.credID...)
	// COSE_Key {1: 1 (OKP), 3: -8 (EdDSA), -1: 6 (Ed25519), -2: x}
	cose := []byte{0xa4, 0x01, 0x01, 0x03, 0x27, 0x20, 0x06, 0x21, 0x58, 0x20}
	auth = append(auth, append(cose, a.priv.Public().(ed25519.PublicKey)...)...)
	// {"fmt": "none", "attStmt": {}, "authData": auth}
	att := []byte{0xa3, 0x63, 'f', 'm', 't', 0x64, 'n', 'o', 'n', 'e', 0x67, 'a', 't', 't', 'S', 't', 'm', 't', 0xa0,
		0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x59}
	att = binary.BigEndian.AppendUint16(att, uint16(len(auth)))
	return clientData, append(att, auth...)
}

// confirm answers navigator.credentials.get for the confirm challenge of digest.
func (a *localAuthenticator) confirm(digest, origin string) (authData, clientData, sig string) {
	cd := []byte(`{"type":"webauthn.get","challenge":"` + b64.EncodeToString(ConfirmChallenge(digest)) + `","origin":"` + origin + `","crossOrigin":false}`)
	rp := sha256.Sum256([]byte(LocalRPID))
	ad := binary.BigEndian.AppendUint32(append(rp[:], webauthnFlagUP|webauthnFlagUV), 0)
	cs := sha256.Sum256(cd)
	return b64.EncodeToString(ad), b64.EncodeToString(cd), b64.EncodeToString(ed25519.Sign(a.priv, append(append([]byte{}, ad...), cs[:]...)))
}

// The local passkey a registration attests is the one that confirms later.
func TestLocalRegistrationYieldsTheConfirmingKey(t *testing.T) {
	a := newLocalAuthenticator()
	challenge := []byte("registration challenge 32 bytes!")
	cd, att := a.register(challenge, "http://localhost:51234")
	key, err := VerifyLocalRegistration(challenge, cd, att)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := x509.MarshalPKIXPublicKey(a.priv.Public())
	if key.SPKI != b64.EncodeToString(want) || key.Alg != -8 || key.RPID != LocalRPID {
		t.Fatalf("registered %+v", key)
	}
	ad, cdj, sig := a.confirm("sha256:abc", "http://127.0.0.1:51234")
	if err := VerifyLocalConfirm(key, "sha256:abc", ad, cdj, sig); err != nil {
		t.Fatalf("the registered key's confirmation does not verify: %v", err)
	}
}

// A signed task on a local binding is held, not run: the server hears
// pending_local, hello lists its digest, a confirmation of another digest is
// refused, the confirmation of the digest shown runs exactly the signed
// command once, and a new process (a restart) holds nothing.
func TestLocalBindingHoldsUntilConfirmed(t *testing.T) {
	view, key := goldenView(t)
	b := view.Bindings["pi-demo"]
	b.Consent = "local"
	a := newLocalAuthenticator()
	cd, att := a.register([]byte("c"), "http://localhost:1")
	local, err := VerifyLocalRegistration([]byte("c"), cd, att)
	if err != nil {
		t.Fatal(err)
	}
	var ran []*protocol.Command
	clk := newClock()
	clk.ns.Store(time.Date(2026, 10, 9, 14, 2, 10, 0, time.UTC).UnixNano())
	newCarrier := func() *Carrier {
		dk, err := MintDeviceKey(t.TempDir(), "example")
		if err != nil {
			t.Fatal(err)
		}
		c, err := New(Config{Name: "example", Pin: Pin{URL: "wss://link.example.test/link", ServerID: testServerID}, StoreID: view.StoreID, Key: dk,
			Now: clk.now, ConsentKeys: func() []ConsentKey { return []ConsentKey{key} }, Bindings: func() []Binding { return []Binding{b} },
			LocalKey: func() (ConsentKey, bool) { return local, true },
			Handle: func(cmd *protocol.Command, _ core.Source) (any, error) {
				ran = append(ran, cmd)
				return protocol.Reply{Outcome: protocol.Outcome{Op: cmd.Op, Evidence: "submitted"}}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := newCarrier()
	var frame struct {
		Frame struct {
			Body SignedSubmit `json:"body"`
		} `json:"frame"`
	}
	if err := json.Unmarshal(readLinkFixture(t, "frames/04-signed_submit.json"), &frame); err != nil {
		t.Fatal(err)
	}
	out, err := c.admitSigned(frame.Frame.Body)
	if r, _ := out.(outcomeReply); err != nil || r.Outcome.Code != protocol.CodePendingLocal || len(ran) != 0 {
		t.Fatalf("admit = %+v, %v, ran %d; want pending_local and nothing run", out, err, len(ran))
	}
	held := c.HeldTasks()
	if len(held) != 1 || c.held.digests(clk.now())[0] != held[0].Digest {
		t.Fatalf("held %+v; want one task listed by digest", held)
	}
	ad, cdj, sig := a.confirm("sha256:0000", "http://localhost:4242")
	if _, err := c.ConfirmLocal(held[0].ID, "sha256:0000", ad, cdj, sig); err == nil || len(ran) != 0 {
		t.Fatal("a confirmation of another digest ran the task")
	}
	ad, cdj, sig = a.confirm(held[0].Digest, "http://localhost:4242")
	if _, err := c.ConfirmLocal(held[0].ID, held[0].Digest, ad, cdj, sig); err != nil || len(ran) != 1 || ran[0].Input.Text != held[0].Text {
		t.Fatalf("confirm = %v, ran %d; want the signed command once", err, len(ran))
	}
	if _, err := c.ConfirmLocal(held[0].ID, held[0].Digest, ad, cdj, sig); err == nil || len(ran) != 1 {
		t.Fatal("a second confirmation ran the task again")
	}
	if _, err := c.admitSigned(frame.Frame.Body); err != nil {
		t.Fatal(err)
	}
	if restarted := newCarrier(); len(restarted.held.digests(clk.now())) != 0 {
		t.Fatal("a new process still lists a held digest")
	}
	var r *Refusal
	if _, err := newCarrier().ConfirmLocal(held[0].ID, held[0].Digest, ad, cdj, sig); !errors.As(err, &r) {
		t.Fatalf("confirm after a restart = %v, want a refusal", err)
	}
}
