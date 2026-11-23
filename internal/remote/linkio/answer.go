package linkio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

const (
	SchemaAnswerInfo      = "amq.remote.link.answer_info/1"
	SchemaSignedAnswer    = "amq.remote.link.signed_answer/1"
	SchemaAnswer          = "amq.remote.answer/1"
	CodeAnswerUnsupported = "answer_unsupported"
)

var assertionFieldRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// time.Parse also accepts a comma for the decimal separator, one-digit hours
// and out-of-range numeric zone offsets. The answer wire uses RFC 3339.
var answerTimestampRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)
var interactionDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// answerDocument is the exact signed content. Every key is required by decodeJCS.
type answerDocument struct {
	Schema            string `json:"schema"`
	ServerID          string `json:"server_id"`
	StoreID           string `json:"store_id"`
	Binding           string `json:"binding"`
	TargetID          string `json:"target_id"`
	Epoch             string `json:"epoch"`
	NativeSessionID   string `json:"native_session_id"`
	Labels            Labels `json:"labels"`
	RequestRef        string `json:"request_ref"`
	InteractionID     string `json:"interaction_id"`
	InteractionDigest string `json:"interaction_digest"`
	Option            string `json:"option"`
	IssuedAt          string `json:"issued_at"`
	NotAfter          string `json:"not_after"`
}

// AnswerEndpoint owns the current share policy and the durable answer write.
type AnswerEndpoint interface {
	LinkAnswerInfo(core.Source, string) (bool, string)
	AnswerLinkDialog(core.Source, core.LinkAnswer) (protocol.Code, string)
}

type answerInfo struct {
	Binding   string `json:"binding"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Now       string `json:"now"`
}

// The signed assertion has the same fields as a signed submit. The frame
// decoder checks its schema. A signed document's type is its exact key set
// together with its schema string; verification requires both.
func verifyAnswer(view *ConsentView, m SignedSubmit, now time.Time) (*answerDocument, error) {
	// The base64 decoder tolerates line breaks, but the link schema admits
	// only the unpadded base64url alphabet in every assertion field.
	for _, field := range []string{m.DocumentB64, m.CredentialID, m.AuthenticatorData, m.ClientDataJSON, m.Signature} {
		if !assertionFieldRE.MatchString(field) {
			return nil, refuse(CodeConsentInvalid, "invalid base64url assertion field")
		}
	}
	raw, _, err := verifySigned(view, m.DocumentB64, m.CredentialID, m.AuthenticatorData, m.ClientDataJSON, m.Signature)
	if err != nil {
		return nil, err
	}
	var d answerDocument
	if err := decodeJCS(raw, &d); err != nil {
		return nil, refuse(CodeConsentInvalid, "%v", err)
	}
	if !protocol.ValidTargetID(d.Binding) || !protocol.ValidTargetID(d.ServerID) || !protocol.ValidTargetID(d.StoreID) ||
		!protocol.ValidTargetID(d.TargetID) || !protocol.ValidEpoch(d.Epoch) {
		return nil, refuse(CodeConsentInvalid, "invalid link or binding identity")
	}
	if d.NativeSessionID == "" || utf8.RuneCountInString(d.NativeSessionID) > 256 ||
		utf8.RuneCountInString(d.Labels.Device) > 128 || utf8.RuneCountInString(d.Labels.Session) > 128 ||
		utf8.RuneCountInString(d.Labels.Harness) > 64 || utf8.RuneCountInString(d.Labels.Project) > 128 {
		return nil, refuse(CodeConsentInvalid, "invalid session or binding labels")
	}
	if d.Schema != SchemaAnswer || d.InteractionID == "" || utf8.RuneCountInString(d.InteractionID) > 128 ||
		!interactionDigestRE.MatchString(d.InteractionDigest) || d.Option == "" || utf8.RuneCountInString(d.Option) > protocol.MaxOptionLen {
		return nil, refuse(CodeConsentInvalid, "invalid signed answer document")
	}
	if _, _, _, err := protocol.DecodeRef(d.RequestRef); err != nil {
		return nil, refuse(CodeConsentInvalid, "invalid signed request_ref")
	}
	if d.ServerID != view.ServerID || d.StoreID != view.StoreID {
		return nil, refuse(CodeConsentInvalid, "the answer is for another server or store")
	}
	if _, err := matchBinding(view, d.Binding, d.TargetID, d.Epoch, d.Labels, d.NativeSessionID); err != nil {
		return nil, err
	}
	if !answerTimestampRE.MatchString(d.IssuedAt) || !answerTimestampRE.MatchString(d.NotAfter) {
		return nil, refuse(CodeConsentInvalid, "answer timestamps must be RFC 3339")
	}
	if err := checkConsentTime(d.IssuedAt, d.NotAfter, now, passkeyMaxLifetime); err != nil {
		return nil, err
	}
	return &d, nil
}

func (c *Carrier) handleAnswerRequest(sess *session, f Frame) {
	if bodySchema(f.Body) == SchemaAnswerInfo {
		var m GrantInfoRequest // both queries carry only schema and binding
		if err := decodeStrict(f.Body, SchemaAnswerInfo, &m); err != nil || !protocol.ValidTargetID(m.Binding) {
			c.refuse(sess, f.ID, ErrorBody{Code: string(protocol.CodeInvalid), Message: "answer_info needs one binding"})
			return
		}
		c.enqueue(sess, f, "answer_info:"+m.Binding, func() (any, error) {
			view := c.view()
			b, ok := view.Bindings[m.Binding]
			if !ok {
				return nil, refuse(string(protocol.CodeUnshared), "binding %q is not shared with this link", m.Binding)
			}
			info := answerInfo{Binding: m.Binding, Reason: "approvals_terminal", Now: c.cfg.Now().UTC().Format(time.RFC3339)}
			if c.cfg.Answers != nil {
				src := c.source(view, b.NativeSessionID, "", m.Binding)
				info.Available, info.Reason = c.cfg.Answers.LinkAnswerInfo(src, m.Binding)
			}
			return struct {
				Info answerInfo `json:"answer_info"`
			}{info}, nil
		})
		return
	}
	var m SignedSubmit
	if err := decodeStrict(f.Body, SchemaSignedAnswer, &m); err != nil {
		c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: err.Error()})
		return
	}
	// Only the dispatch order depends on this unverified reference. A valid
	// answer queues with submits and cancels for the same request.
	var route struct {
		RequestRef string `json:"request_ref"`
	}
	doc, err := b64.DecodeString(m.DocumentB64)
	if err != nil || json.Unmarshal(doc, &route) != nil {
		c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: "the signed document names no request"})
		return
	}
	_, _, id, err := protocol.DecodeRef(route.RequestRef)
	if err != nil {
		c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: "invalid signed request_ref"})
		return
	}
	c.enqueue(sess, f, id, func() (any, error) {
		view := c.view()
		d, err := verifyAnswer(view, m, c.cfg.Now())
		if err != nil {
			return nil, err
		}
		if c.cfg.Answers == nil {
			return nil, refuse(CodeAnswerUnsupported, "binding %q takes answers at the terminal", d.Binding)
		}
		sum := sha256.Sum256(doc)
		answer := core.LinkAnswer{
			RequestRef:        d.RequestRef,
			InteractionID:     d.InteractionID,
			InteractionDigest: d.InteractionDigest,
			Option:            d.Option,
			DocumentDigest:    "sha256:" + hex.EncodeToString(sum[:]),
			Credential:        m.CredentialID,
		}
		// Core trusts this source. The carrier owns the authenticated host, and
		// verifyAnswer matched the signed binding and native session to this view.
		src := c.source(view, d.NativeSessionID, m.CredentialID, d.Binding)
		if code, message := c.cfg.Answers.AnswerLinkDialog(src, answer); code != "" {
			return nil, refuse(string(code), "%s", message)
		}
		return map[string]any{"answer": map[string]string{"request_ref": d.RequestRef, "interaction_id": d.InteractionID, "state": "recorded"}}, nil
	})
}
