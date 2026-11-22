package linkio

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// SchemaGrant is the signed session grant document.
const SchemaGrant = "amq.remote.grant/1"

// GrantMaxLifetime is the longest a grant may last: not_after - issued_at.
const GrantMaxLifetime = 24 * time.Hour

var grantIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// GrantDocument is amq.remote.grant/1: what the owner signed to let the
// link send tasks to one binding with no signature per task. Every key is
// required and no other is read, so no field has omitempty.
type GrantDocument struct {
	Schema          string     `json:"schema"`
	ServerID        string     `json:"server_id"`
	StoreID         string     `json:"store_id"`
	GrantID         string     `json:"grant_id"`
	Binding         string     `json:"binding"`
	TargetID        string     `json:"target_id"`
	Epoch           string     `json:"epoch"`
	NativeSessionID string     `json:"native_session_id"`
	Labels          Labels     `json:"labels"`
	GrantGeneration string     `json:"grant_generation"`
	IssuedAt        string     `json:"issued_at"`
	NotAfter        string     `json:"not_after"`
	Input           GrantInput `json:"input"`
}

// GrantInput is the one class of input a grant covers.
type GrantInput struct {
	Busy        string `json:"busy"`
	Deliver     string `json:"deliver"`
	MinEvidence string `json:"min_evidence"`
}

// GrantState is what the machine holds for the binding a signed grant
// names, read before the grant is verified. Its zero value refuses.
type GrantState struct {
	Generation string       // the binding's grant generation now
	Stored     *StoredGrant // the grant stored for the binding; nil when none
	Available  bool         // the harness reports now that the session asks
	Reason     string       // why it is not available (GrantReason*)
}

// DecodeGrant decodes the signed bytes of a grant strictly (decodeJCS) and
// checks its schema and grant id.
func DecodeGrant(doc []byte) (*GrantDocument, error) {
	var d GrantDocument
	if err := decodeJCS(doc, &d); err != nil {
		return nil, err
	}
	if d.Schema != SchemaGrant {
		return nil, fmt.Errorf("schema %q, want %s", d.Schema, SchemaGrant)
	}
	if !grantIDRe.MatchString(d.GrantID) {
		return nil, fmt.Errorf("grant_id %q is not a lowercase UUID", d.GrantID)
	}
	return &d, nil
}

// VerifySignedGrant decides whether a signed grant may be stored, and
// returns the grant to store. Nothing else in the frame is trusted. A
// presentation of the stored grant's exact bytes skips only the issued_at
// window and answers the stored grant; every other check still runs.
func VerifySignedGrant(view *ConsentView, m SignedGrant, st GrantState, now time.Time) (StoredGrant, error) {
	doc, _, err := verifySigned(view, m.DocumentB64, m.CredentialID, m.AuthenticatorData, m.ClientDataJSON, m.Signature)
	if err != nil {
		return StoredGrant{}, err
	}
	d, err := DecodeGrant(doc)
	if err != nil {
		return StoredGrant{}, refuse(CodeConsentInvalid, "%v", err)
	}
	b, shared := view.Bindings[d.Binding]
	switch {
	case d.ServerID != view.ServerID || d.StoreID != view.StoreID:
		return StoredGrant{}, refuse(CodeConsentInvalid, "the grant is for another server or store")
	case !shared:
		return StoredGrant{}, refuse(string(protocol.CodeUnshared), "binding %q is not shared with this link", d.Binding)
	case d.TargetID != b.TargetID || d.Epoch != b.Epoch || d.Labels != b.Labels:
		return StoredGrant{}, refuse(string(protocol.CodeStaleEpoch), "the binding changed since you signed; read it again and sign again")
	case d.NativeSessionID == "" || d.NativeSessionID != b.NativeSessionID:
		return StoredGrant{}, refuse(CodeSessionChanged, "the session behind %s changed since you signed", d.Binding)
	case b.Consent != "passkey":
		return StoredGrant{}, refuse(CodeGrantUnsupported, "%s: binding %q takes no grant: its consent is %q", GrantReasonConsentLocal, d.Binding, b.Consent)
	case !st.Available:
		reason := st.Reason
		if reason == "" {
			reason = GrantReasonModeUnreadable
		}
		return StoredGrant{}, refuse(CodeGrantUnsupported, "%s: binding %q takes no grant now", reason, d.Binding)
	case st.Generation == "" || d.GrantGeneration != st.Generation:
		return StoredGrant{}, refuse(CodeGrantRevoked, "binding %q has another grant generation now; ask grant_info and sign again", d.Binding)
	case d.Input.Busy != string(protocol.BusyReject) || d.Input.Deliver != string(protocol.DeliverTurn) ||
		(d.Input.MinEvidence != protocol.EvidenceSubmitted && d.Input.MinEvidence != protocol.EvidenceAdmitted):
		return StoredGrant{}, refuse(CodeConsentInvalid, "a grant covers only busy reject, deliver turn and min_evidence submitted or admitted")
	}
	issued, err := time.Parse(time.RFC3339, d.IssuedAt)
	if err != nil {
		return StoredGrant{}, refuse(CodeConsentInvalid, "issued_at is not RFC 3339")
	}
	notAfter, err := time.Parse(time.RFC3339, d.NotAfter)
	if err != nil {
		return StoredGrant{}, refuse(CodeConsentInvalid, "not_after is not RFC 3339")
	}
	if life := notAfter.Sub(issued); life <= 0 || life > GrantMaxLifetime {
		return StoredGrant{}, refuse(CodeGrantTooLong, "the grant's lifetime %s is outside (0, %s]", life, GrantMaxLifetime)
	}
	sum := sha256.Sum256(doc)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	stored := st.Stored != nil && st.Stored.Digest == digest && st.Stored.Binding == d.Binding
	if !stored {
		switch {
		case issued.After(now.Add(consentClockWindow)):
			return StoredGrant{}, refuse(CodeClockSkew, "check your clock: the grant was issued at %s, this machine says %s", d.IssuedAt, now.UTC().Format(time.RFC3339))
		case issued.Before(now.Add(-consentClockWindow)):
			return StoredGrant{}, refuse(CodeGrantStale, "the grant was issued at %s, more than %s before this machine's %s; sign again", d.IssuedAt, consentClockWindow, now.UTC().Format(time.RFC3339))
		}
	}
	if !now.Before(notAfter) {
		return StoredGrant{}, refuse(CodeGrantExpired, "the grant expired at %s", d.NotAfter)
	}
	if stored {
		return *st.Stored, nil
	}
	return StoredGrant{
		GrantID: d.GrantID, Digest: digest, CredentialID: m.CredentialID, Binding: d.Binding,
		TargetID: d.TargetID, Epoch: d.Epoch, NativeSessionID: d.NativeSessionID, Labels: d.Labels,
		Generation: d.GrantGeneration, IssuedAt: d.IssuedAt, NotAfter: d.NotAfter, MinEvidence: d.Input.MinEvidence,
	}, nil
}
