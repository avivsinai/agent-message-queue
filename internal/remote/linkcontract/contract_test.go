package linkcontract

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

const schemaBase = "https://github.com/avivsinai/agent-message-queue/schemas/"

type frameFile struct {
	Direction string          `json:"direction"`
	BodyDef   string          `json:"body_def"`
	Frame     json.RawMessage `json:"frame"`
}

type envelope struct {
	ID   string          `json:"id"`
	Re   string          `json:"re"`
	Gen  *int64          `json:"gen"`
	Body json.RawMessage `json:"body"`
}

type assertionFile struct {
	CredentialID      string `json:"credential_id"`
	SPKI              string `json:"spki"`
	Alg               int    `json:"alg"`
	RPID              string `json:"rp_id"`
	Origin            string `json:"origin"`
	BackupEligible    bool   `json:"backup_eligible"`
	Fingerprint       string `json:"fingerprint"`
	Challenge         string `json:"challenge"`
	AuthenticatorData string `json:"authenticator_data"`
	ClientDataJSON    string `json:"client_data_json"`
	Signature         string `json:"signature"`
}

// TestFramesMatchSchema validates every golden frame: the envelope, its body
// against the definition the file names, and the gen rule.
func TestFramesMatchSchema(t *testing.T) {
	c := jsonschema.NewCompiler()
	for _, name := range []string{"remote-link-v1", "remote-command-v1", "remote-request-v1", "remote-session-v1"} {
		f, err := os.Open(filepath.Join("..", "..", "..", "schemas", name+".schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(schemaBase+name+".schema.json", doc); err != nil {
			t.Fatal(err)
		}
	}
	root, err := c.Compile(schemaBase + "remote-link-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	frames := readFrames(t)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	for name, ff := range frames {
		body, err := c.Compile(schemaBase + "remote-link-v1.schema.json#/$defs/" + ff.BodyDef)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		frame := mustUnmarshalAny(t, ff.Frame)
		if err := root.Validate(frame); err != nil {
			t.Errorf("%s: envelope: %v", name, err)
		}
		if err := body.Validate(frame.(map[string]any)["body"]); err != nil {
			t.Errorf("%s: body %s: %v", name, ff.BodyDef, err)
		}
		env := decodeEnvelope(t, ff.Frame)
		preWelcome := ff.BodyDef == "challenge" || ff.BodyDef == "hello"
		if preWelcome != (env.Gen == nil) {
			t.Errorf("%s: gen must be absent exactly on challenge and hello", name)
		}
	}
}

// TestSignedConsent checks the golden consent document and its assertion the
// way a verifier does (design section 6), against the derived test keys.
func TestSignedConsent(t *testing.T) {
	doc := readFile(t, "consent/document.json")
	canon, err := jcs.Canonicalize(doc)
	if err != nil || !bytes.Equal(canon, doc) {
		t.Fatalf("document.json is not its own JCS form: %v", err)
	}
	var a assertionFile
	mustUnmarshal(t, readFile(t, "consent/assertion.json"), &a)

	docSum := sha256.Sum256(doc)
	if a.Challenge != b64.EncodeToString(docSum[:]) {
		t.Fatal("challenge is not SHA-256(document bytes)")
	}
	clientData := mustB64(t, a.ClientDataJSON)
	var cd struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
		TopOrigin   string `json:"topOrigin"`
	}
	mustUnmarshal(t, clientData, &cd)
	if cd.Type != "webauthn.get" || cd.Challenge != a.Challenge || cd.Origin != a.Origin || cd.CrossOrigin || cd.TopOrigin != "" {
		t.Fatalf("clientDataJSON does not match the profile: %+v", cd)
	}
	authData := mustB64(t, a.AuthenticatorData)
	rpHash := sha256.Sum256([]byte(a.RPID))
	if len(authData) != 37 || !bytes.Equal(authData[:32], rpHash[:]) {
		t.Fatal("authenticatorData does not start with SHA-256(rp_id)")
	}
	if flags := authData[32]; flags&0x05 != 0x05 || (flags&0x08 != 0) != a.BackupEligible {
		t.Fatalf("flags %#x: want UP and UV, BE as recorded", flags)
	}

	pub, err := x509.ParsePKIXPublicKey(mustB64(t, a.SPKI))
	if err != nil {
		t.Fatal(err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok || !ecPub.Equal(&consentKey(t).PublicKey) || a.Alg != -7 {
		t.Fatal("spki is not the ES256 key derived from the consent seed")
	}
	clientSum := sha256.Sum256(clientData)
	signed := sha256.Sum256(append(append([]byte{}, authData...), clientSum[:]...))
	if !ecdsa.VerifyASN1(ecPub, signed[:], mustB64(t, a.Signature)) {
		t.Fatal("ES256 signature does not verify")
	}
	if a.CredentialID != credentialID() {
		t.Fatal("credential_id is not the derived test credential")
	}
	if got := fingerprint(t, a.CredentialID, mustB64(t, a.SPKI), a.Alg, a.RPID, a.Origin); got != a.Fingerprint {
		t.Fatalf("fingerprint = %s, file says %s", got, a.Fingerprint)
	}

	// The command inside the document is an ordinary, valid submit.
	var d struct {
		Command json.RawMessage `json:"command"`
	}
	mustUnmarshal(t, doc, &d)
	if _, err := protocol.DecodeCommand(d.Command); err != nil {
		t.Fatalf("document.command: %v", err)
	}

	// signed_submit carries exactly these bytes and this assertion.
	var ss struct {
		DocumentB64       string `json:"document_b64"`
		CredentialID      string `json:"credential_id"`
		AuthenticatorData string `json:"authenticator_data"`
		ClientDataJSON    string `json:"client_data_json"`
		Signature         string `json:"signature"`
	}
	mustUnmarshal(t, bodyOf(t, "signed_submit"), &ss)
	if !bytes.Equal(mustB64(t, ss.DocumentB64), doc) || ss.CredentialID != a.CredentialID ||
		ss.AuthenticatorData != a.AuthenticatorData || ss.ClientDataJSON != a.ClientDataJSON || ss.Signature != a.Signature {
		t.Fatal("signed_submit does not carry the golden document and assertion")
	}
}

// TestHelloAndRevisions checks the device signature, the binding the consent
// was signed for, and the revision digests and acknowledgement.
func TestHelloAndRevisions(t *testing.T) {
	var dev struct {
		DeviceKey   string `json:"device_key"`
		CreatorHost string `json:"creator_host"`
	}
	mustUnmarshal(t, readFile(t, "device.json"), &dev)
	devSPKI := mustB64(t, dev.DeviceKey)
	if !bytes.Equal(devSPKI, spki(t, deviceKey().Public())) || dev.CreatorHost != creatorHost(devSPKI) {
		t.Fatal("device.json does not match the derived device key")
	}

	var ch struct {
		ServerID string `json:"server_id"`
		Nonce    string `json:"nonce"`
	}
	mustUnmarshal(t, bodyOf(t, "challenge"), &ch)
	var hello struct {
		DeviceKey string            `json:"device_key"`
		StoreID   string            `json:"store_id"`
		Signature string            `json:"signature"`
		Bindings  []json.RawMessage `json:"bindings"`
	}
	mustUnmarshal(t, bodyOf(t, "hello"), &hello)
	pub := ed25519.PublicKey(deviceKey().Public().(ed25519.PublicKey))
	if hello.DeviceKey != dev.DeviceKey ||
		!ed25519.Verify(pub, helloMessage(ch.ServerID, ch.Nonce, hello.StoreID), mustB64(t, hello.Signature)) {
		t.Fatal("hello signature does not verify under the device key")
	}

	// The consent was signed for the binding hello reports.
	var doc struct {
		ServerID        string          `json:"server_id"`
		StoreID         string          `json:"store_id"`
		Binding         string          `json:"binding"`
		NativeSessionID string          `json:"native_session_id"`
		Labels          json.RawMessage `json:"labels"`
		Command         struct {
			TargetID string `json:"target_id"`
			Epoch    string `json:"epoch"`
		} `json:"command"`
	}
	mustUnmarshal(t, readFile(t, "consent/document.json"), &doc)
	var b struct {
		Binding         string          `json:"binding"`
		TargetID        string          `json:"target_id"`
		Epoch           string          `json:"epoch"`
		NativeSessionID string          `json:"native_session_id"`
		Labels          json.RawMessage `json:"labels"`
	}
	mustUnmarshal(t, hello.Bindings[0], &b)
	if doc.ServerID != ch.ServerID || doc.StoreID != hello.StoreID || doc.Binding != b.Binding ||
		doc.Command.TargetID != b.TargetID || doc.Command.Epoch != b.Epoch || doc.NativeSessionID != b.NativeSessionID ||
		!bytes.Equal(mustJCSRaw(t, doc.Labels), mustJCSRaw(t, b.Labels)) {
		t.Fatal("the consent document does not match the binding in hello")
	}

	for _, name := range []string{"revision_interaction", "revision_completed"} {
		var r struct {
			StoreID    string          `json:"store_id"`
			RequestRef string          `json:"request_ref"`
			Revision   int64           `json:"revision"`
			Digest     string          `json:"digest"`
			Snapshot   json.RawMessage `json:"snapshot"`
		}
		mustUnmarshal(t, bodyOf(t, name), &r)
		sum := sha256.Sum256(mustJCSRaw(t, r.Snapshot))
		if r.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Errorf("%s: digest is not sha256 of the snapshot's JCS form", name)
		}
		var s protocol.Snapshot
		mustUnmarshal(t, r.Snapshot, &s)
		if s.Revision != r.Revision || s.RequestRef != r.RequestRef || r.StoreID != hello.StoreID ||
			s.CreatorHost != dev.CreatorHost || s.RequestRef != protocol.EncodeRef(dev.CreatorHost, s.TargetID, s.RequestID) {
			t.Errorf("%s: revision and snapshot disagree", name)
		}
	}
	var ack struct {
		OK string `json:"ok"`
	}
	mustUnmarshal(t, bodyOf(t, "revision_ack"), &ack)
	var done struct {
		Digest string `json:"digest"`
	}
	mustUnmarshal(t, bodyOf(t, "revision_completed"), &done)
	if ack.OK != done.Digest || envelopeOf(t, "revision_ack").Re != envelopeOf(t, "revision_completed").ID {
		t.Fatal("the ack does not answer the completed revision with its digest")
	}
}

func readFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(linkDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// readFrames maps each frame's name (file name without the NN- prefix) to it.
func readFrames(t *testing.T) map[string]frameFile {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(linkDir, "frames", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	out := map[string]frameFile{}
	for _, p := range paths {
		var ff frameFile
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		mustUnmarshal(t, raw, &ff)
		base := strings.TrimSuffix(filepath.Base(p), ".json")
		out[base[strings.IndexByte(base, '-')+1:]] = ff
	}
	return out
}

func envelopeOf(t *testing.T, name string) envelope {
	t.Helper()
	ff, ok := readFrames(t)[name]
	if !ok {
		t.Fatalf("no frame %s", name)
	}
	return decodeEnvelope(t, ff.Frame)
}

func bodyOf(t *testing.T, name string) []byte { return envelopeOf(t, name).Body }

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	mustUnmarshal(t, raw, &env)
	return env
}

func mustJCSRaw(t *testing.T, raw []byte) []byte {
	t.Helper()
	b, err := jcs.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustUnmarshal(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func mustUnmarshalAny(t *testing.T, raw []byte) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
