package core

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// LinkAnswer is a signed answer whose document the link carrier verified:
// the fields of the document the endpoint admits, after the wire side's
// checks ran (contract section 6, steps 1 to 3). The signature proves the
// holder of the passkey signed this dialog, its text, its choices with
// their roles, and one choice; the wire side owns the document itself.
type LinkAnswer struct {
	RequestRef        string
	InteractionID     string
	InteractionDigest string // "sha256:..." as signed
	Option            string // a label: the interaction's approve_option or reject_option
	DocumentDigest    string // "sha256:" + hex of the signed document bytes
	Credential        string // the credential id that signed
}

// AnswerLinkDialog admits a verified answer to the dialog a link's running
// request publishes: contract section 6, checks 4 to 7. It returns "" after
// the answer file and its directory entry are durable. Any other return is
// the refusal code and a message; nothing was recorded, except for
// internal, where the machine could not confirm durability.
func (e *Endpoint) AnswerLinkDialog(src Source, a LinkAnswer) (protocol.Code, string) {
	// Step 4: the share takes link answers, and its consent is passkey.
	// The machine's judgment, asked fresh — a policy that moved since the
	// dialog was published stops answers now.
	if e.linkDialogs == nil || src.Binding == "" {
		return protocol.CodeAnswerUnsupported, "no shared binding takes link answers"
	}
	if available, reason := e.linkDialogs.AnswerPolicy(src.Host, src.Binding); !available {
		return protocol.CodeAnswerUnsupported, reason
	}

	// Step 5: the ref names a request this link submitted on this binding,
	// still running. The binding name comes from the record; a record
	// without it takes no link answer.
	creatorHost, targetID, requestID, err := protocol.DecodeRef(a.RequestRef)
	if err != nil {
		return protocol.CodeNotFound, err.Error()
	}
	if creatorHost != src.Host {
		return protocol.CodeNotFound, "the request_ref names another link's request"
	}
	rec, exists, err := e.store.Get(requests.Key{CreatorHost: creatorHost, TargetID: targetID, RequestID: requestID})
	if err != nil {
		return protocol.CodeInternal, err.Error()
	}
	if !exists || rec.State != protocol.StateRunning {
		return protocol.CodeNotFound, "no running request for the request_ref"
	}
	if rec.Binding == "" || rec.Binding != src.Binding {
		return protocol.CodeNotFound, "the request does not run on this binding"
	}
	// The answer document names the verified native session; a record in
	// another session, or one whose binding moved to another target or
	// generation, takes no link answer.
	if rec.NativeSession == "" || rec.NativeSession != src.NativeSession {
		return protocol.CodeNotFound, "the request does not run in the verified native session"
	}
	if !e.linkDialogs.RequestLive(rec) {
		return protocol.CodeNotFound, "the request's session is no longer live"
	}

	// Step 6: the record's pending interaction is this dialog, still live
	// for a link answer, and unchanged since the touch. The answer file is
	// the only truth about "already answered": the exclusive create
	// decides, so an already-recorded answer wins ahead of every digest
	// and option judgment, and a stored flag the seam has since
	// invalidated — a bad answer removed — never blocks re-admission.
	if rec.Interaction == nil || rec.Interaction.InteractionID != a.InteractionID ||
		!isLinkDialog(rec, a.InteractionID) {
		return protocol.CodeAlreadyResolved, "the dialog is not waiting for a link answer"
	}
	if e.linkDialogs.HasAnswer(src.Binding, a.InteractionID) {
		return protocol.CodeAlreadyResolved, "the dialog's answer is recorded"
	}
	want, err := interactionDigest(rec.Interaction)
	if err != nil {
		return protocol.CodeInternal, err.Error()
	}
	if a.InteractionDigest != want {
		return protocol.CodeDialogChanged, "the signed dialog no longer matches the published interaction"
	}
	if a.Option == "" || (a.Option != rec.Interaction.ApproveOption && a.Option != rec.Interaction.RejectOption) {
		return protocol.CodeInvalid, "the option is not one of the dialog's link choices"
	}

	// Step 7: the exclusive, directory-bound write, with the choice's
	// expected role as the stored interaction carries it and the signed
	// digest for the write's own re-render check. The seam owns the
	// files; a refusal here records nothing.
	role := dialog.RoleApprove
	if a.Option == rec.Interaction.RejectOption {
		role = dialog.RoleDeny
	}
	code, msg := e.linkDialogs.WriteAnswer(src.Host, src.Binding, a.InteractionID, a.Option, role, want, a.DocumentDigest, a.Credential)
	if code != "" {
		return code, msg
	}
	return "", ""
}

// interactionDigest is the value the server signed: "sha256:" + hex of the
// JCS bytes of the interaction's identity — exactly interaction_id, kind,
// options, prompt and, when present, approve_option and reject_option. The
// remote_answer flag is part of the revision, never of the digest: an
// answer file appearing must not make signed documents stale.
func interactionDigest(in *protocol.Interaction) (string, error) {
	cp := *in
	cp.RemoteAnswer = false
	canon, err := jcs.Marshal(cp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// InteractionDigest is the value a server signs for one interaction: the
// identity keys' JCS hash, the remote_answer flag excluded. The machine's
// answer seam re-renders the dialog's record file and compares this
// digest against the signed one before it writes — the one rule for what
// the signature covers, shared by admission and the write.
func InteractionDigest(in *protocol.Interaction) (string, error) {
	return interactionDigest(in)
}

// LinkAnswerInfo reports whether the binding's share takes link answers
// now, and the reason when it does not: approvals_terminal, consent_local
// or harness_unsupported, in that order when more than one holds. The
// wire side refuses an unshared binding before it calls.
func (e *Endpoint) LinkAnswerInfo(src Source, binding string) (available bool, reason string) {
	if e.linkDialogs == nil || binding == "" {
		return false, "approvals_terminal"
	}
	return e.linkDialogs.AnswerPolicy(src.Host, binding)
}
