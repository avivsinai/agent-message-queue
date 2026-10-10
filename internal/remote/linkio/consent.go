package linkio

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// SchemaConsent is the signed consent document.
const SchemaConsent = "amq.remote.consent/2"

// Refusal codes of a signed submit, before admission.
const (
	CodeConsentInvalid = "consent_invalid"
	CodeSessionChanged = "session_changed"
	CodeClockSkew      = "clock_skew"
)

// Time bounds of a consent (design §6).
const (
	consentClockWindow   = 60 * time.Second
	passkeyMaxLifetime   = 2 * time.Minute
	localMaxLifetime     = 10 * time.Minute
	webauthnFlagUP       = 0x01
	webauthnFlagUV       = 0x04
	webauthnFlagBE       = 0x08
	authenticatorDataMin = 37
)

// ConsentDocument is amq.remote.consent/2: what the user signed.
type ConsentDocument struct {
	Schema          string           `json:"schema"`
	ServerID        string           `json:"server_id"`
	StoreID         string           `json:"store_id"`
	Binding         string           `json:"binding"`
	NativeSessionID string           `json:"native_session_id"`
	IssuedAt        string           `json:"issued_at"`
	Labels          Labels           `json:"labels"`
	Command         protocol.Command `json:"command"`
}

// SignedSubmit is the body of amq.remote.link.signed_submit/1.
type SignedSubmit struct {
	Schema            string `json:"schema"`
	DocumentB64       string `json:"document_b64"`
	CredentialID      string `json:"credential_id"`
	AuthenticatorData string `json:"authenticator_data"`
	ClientDataJSON    string `json:"client_data_json"`
	Signature         string `json:"signature"`
}

// ConsentView is what the machine trusts, read as one immutable value: the
// pinned server, its consent keys and the bindings shared with the link.
type ConsentView struct {
	ServerID string
	StoreID  string
	Keys     map[string]ConsentKey // by credential id; a removed key is absent
	Bindings map[string]Binding    // by binding name, as reported to the server
}

// Refusal is a signed submit refused before admission.
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

func refuse(code, format string, args ...any) error {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Verified is a signed submit that passed every check: the command decoded
// from the exact signed bytes, the native session it was signed for, the
// binding it targets and the digest of the signed bytes.
type Verified struct {
	Cmd      *protocol.Command
	Native   string
	Binding  Binding
	Digest   string // "sha256:" + hex of the signed document bytes
	Text     string // the task text, as signed
	NotAfter time.Time
}

// VerifySignedSubmit decides whether a signed submit may run, and returns
// what was signed. Nothing else in the frame is trusted. A binding whose
// consent is local is verified the same way; the caller then holds it for
// the local confirmation instead of running it.
func VerifySignedSubmit(view *ConsentView, m SignedSubmit, now time.Time) (*Verified, error) {
	cmd, native, b, doc, err := verifySignedSubmit(view, m, now)
	if err != nil {
		return nil, err
	}
	notAfter, err := time.Parse(time.RFC3339, cmd.NotAfter)
	if err != nil {
		return nil, refuse(CodeConsentInvalid, "not_after is not RFC 3339")
	}
	sum := sha256.Sum256(doc)
	return &Verified{Cmd: cmd, Native: native, Binding: b, Digest: "sha256:" + hex.EncodeToString(sum[:]), Text: cmd.Input.Text, NotAfter: notAfter}, nil
}

func verifySignedSubmit(view *ConsentView, m SignedSubmit, now time.Time) (*protocol.Command, string, Binding, []byte, error) {
	doc, err := b64.DecodeString(m.DocumentB64)
	if err != nil {
		return nil, "", Binding{}, nil, refuse(CodeConsentInvalid, "the document is not base64url")
	}
	key, ok := view.Keys[m.CredentialID]
	if !ok {
		return nil, "", Binding{}, nil, refuse(CodeConsentInvalid, "the passkey is not one this machine accepts")
	}
	docSum := sha256.Sum256(doc)
	originOK := func(o string) bool { return o == key.Origin }
	if err := verifyWebAuthnGet(key, docSum[:], originOK, m.AuthenticatorData, m.ClientDataJSON, m.Signature); err != nil {
		return nil, "", Binding{}, nil, refuse(CodeConsentInvalid, "%v", err)
	}
	d, err := DecodeConsent(doc)
	if err != nil {
		return nil, "", Binding{}, nil, refuse(CodeConsentInvalid, "%v", err)
	}
	b, shared := view.Bindings[d.Binding]
	switch {
	case d.ServerID != view.ServerID || d.StoreID != view.StoreID:
		return nil, "", Binding{}, nil, refuse(CodeConsentInvalid, "the consent is for another server or store")
	case !shared:
		return nil, "", Binding{}, nil, refuse(string(protocol.CodeUnshared), "binding %q is not shared with this link", d.Binding)
	case d.Command.TargetID != b.TargetID || d.Command.Epoch != b.Epoch || d.Labels != b.Labels:
		return nil, "", Binding{}, nil, refuse(string(protocol.CodeStaleEpoch), "the binding changed since you signed; read it again and sign again")
	case d.NativeSessionID == "" || d.NativeSessionID != b.NativeSessionID:
		return nil, "", Binding{}, nil, refuse(CodeSessionChanged, "the session behind %s changed since you signed", d.Binding)
	}
	issued, err := time.Parse(time.RFC3339, d.IssuedAt)
	if err != nil {
		return nil, "", Binding{}, nil, refuse(CodeConsentInvalid, "issued_at is not RFC 3339")
	}
	notAfter, err := time.Parse(time.RFC3339, d.Command.NotAfter)
	if err != nil {
		return nil, "", Binding{}, nil, refuse(CodeConsentInvalid, "not_after is not RFC 3339")
	}
	maxLife := passkeyMaxLifetime
	if b.Consent == "local" {
		maxLife = localMaxLifetime
	}
	life := notAfter.Sub(issued)
	switch {
	case issued.After(now.Add(consentClockWindow)) || issued.Before(now.Add(-consentClockWindow)):
		return nil, "", Binding{}, nil, refuse(CodeClockSkew, "check your clock: the consent was issued at %s, this machine says %s", d.IssuedAt, now.UTC().Format(time.RFC3339))
	case life <= 0 || life > maxLife:
		return nil, "", Binding{}, nil, refuse(CodeConsentInvalid, "the consent's lifetime %s is outside (0, %s]", life, maxLife)
	case !now.Before(notAfter):
		return nil, "", Binding{}, nil, refuse(string(protocol.CodeExpired), "the consent expired at %s", d.Command.NotAfter)
	}
	cmd := d.Command
	return &cmd, d.NativeSessionID, b, doc, nil
}

// DecodeConsent decodes the signed bytes strictly. The bytes must be their
// own JCS form (no duplicate key, no lone surrogate), the command must be a
// valid submit with its defaults written out, and the decoded document must
// encode back to exactly these bytes. The last rule refuses every key that
// is unknown or differs only by case: encoding/json matches "TEXT" to the
// field "text", so two spellings could otherwise carry two texts.
func DecodeConsent(doc []byte) (*ConsentDocument, error) {
	canon, err := jcs.Canonicalize(doc)
	if err != nil {
		return nil, fmt.Errorf("the document is not canonical JSON: %w", err)
	}
	if !bytes.Equal(canon, doc) {
		return nil, errors.New("the document is not in its canonical (JCS) form")
	}
	var d ConsentDocument
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("the document does not decode: %w", err)
	}
	again, err := jcs.Marshal(d)
	if err != nil || !bytes.Equal(again, doc) {
		return nil, errors.New("the document has a key this machine does not read exactly (unknown, or differing only by case)")
	}
	if d.Schema != SchemaConsent {
		return nil, fmt.Errorf("schema %q, want %s", d.Schema, SchemaConsent)
	}
	if d.Command.Op != protocol.OpRequestSubmit || d.Command.Input == nil ||
		d.Command.Input.Busy == "" || d.Command.Input.Deliver == "" || d.Command.Input.MinEvidence == "" {
		return nil, errors.New("the command is not a submit with busy, deliver and min_evidence written out")
	}
	cmdRaw, err := json.Marshal(d.Command)
	if err != nil {
		return nil, err
	}
	if _, err := protocol.DecodeCommand(cmdRaw); err != nil {
		return nil, fmt.Errorf("the command is invalid: %w", err)
	}
	return &d, nil
}

// verifyWebAuthnGet checks a WebAuthn assertion (L3 §7.2, the design's
// profile) whose challenge must be challenge, made on an origin originOK
// accepts. There is no counter check: synced passkeys report 0.
func verifyWebAuthnGet(key ConsentKey, challenge []byte, originOK func(string) bool, authDataB64, clientDataB64, sigB64 string) error {
	authData, err1 := b64.DecodeString(authDataB64)
	clientData, err2 := b64.DecodeString(clientDataB64)
	sig, err3 := b64.DecodeString(sigB64)
	if err := errors.Join(err1, err2, err3); err != nil {
		return errors.New("the assertion is not base64url")
	}
	var cd struct {
		Type        string          `json:"type"`
		Challenge   string          `json:"challenge"`
		Origin      string          `json:"origin"`
		CrossOrigin bool            `json:"crossOrigin"`
		TopOrigin   json.RawMessage `json:"topOrigin"`
	}
	if err := json.Unmarshal(clientData, &cd); err != nil {
		return errors.New("clientDataJSON does not decode")
	}
	rpHash := sha256.Sum256([]byte(key.RPID))
	switch {
	case cd.Type != "webauthn.get":
		return errors.New("the assertion is not a webauthn.get")
	case cd.Challenge != b64.EncodeToString(challenge):
		return errors.New("the passkey signed other bytes")
	case !originOK(cd.Origin) || cd.CrossOrigin || len(cd.TopOrigin) != 0:
		return errors.New("the passkey was used on another origin")
	case len(authData) < authenticatorDataMin || !bytes.Equal(authData[:32], rpHash[:]):
		return errors.New("the assertion is for another relying party")
	}
	flags := authData[32]
	if flags&webauthnFlagUP == 0 || flags&webauthnFlagUV == 0 {
		return errors.New("the passkey did not verify the user")
	}
	if (flags&webauthnFlagBE != 0) != key.BackupEligible {
		return errors.New("the passkey's backup eligibility differs from enrollment")
	}
	clientSum := sha256.Sum256(clientData)
	signed := append(append([]byte{}, authData...), clientSum[:]...)
	spki, err := b64.DecodeString(key.SPKI)
	if err != nil {
		return errors.New("the stored key is not base64url")
	}
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return errors.New("the stored key does not parse")
	}
	switch key.Alg {
	case -7:
		ec, ok := pub.(*ecdsa.PublicKey)
		sum := sha256.Sum256(signed)
		if !ok || ec.Curve != elliptic.P256() || !ecdsa.VerifyASN1(ec, sum[:], sig) {
			return errors.New("the signature does not verify")
		}
	case -8:
		ed, ok := pub.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(ed, signed, sig) {
			return errors.New("the signature does not verify")
		}
	default:
		return fmt.Errorf("algorithm %d is not accepted", key.Alg)
	}
	return nil
}
