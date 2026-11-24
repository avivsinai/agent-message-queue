package main

import (
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// linkDialogSource implements core.LinkDialogs against a binding's dialog
// directory. It is the machine's own policy: only the exact running link
// that owns a request, through the share that names the request's binding,
// with passkey consent and approvals=link, publishes its dialogs — a dialog
// never qualifies because the server asked for it.
type linkDialogSource struct {
	links *linkSet
}

// sharePolicy resolves the exact link and share that own rec: the running
// link whose carrier the request came from (its creator host), then the
// share naming rec.Binding. The binding must still be this root's live
// native session in the generation the request was admitted in. It returns
// the share's approvals and consent; ok=false when any of that fails.
func (s *linkDialogSource) sharePolicy(rec *requests.Record) (approvals, consent string, ok bool) {
	// The request's signed session anchors every comparison: an unbound
	// record has no identity to publish a dialog under (R1 withholding).
	if rec.NativeSession == "" {
		return "", "", false
	}
	s.links.mu.Lock()
	c := s.links.byHost[rec.CreatorHost]
	var shares []manifest.LinkShare
	for _, r := range s.links.running {
		if r.carrier == c {
			shares = r.link.Shares
		}
	}
	ep, root := s.links.ep, s.links.root
	s.links.mu.Unlock()
	if c == nil || ep == nil {
		return "", "", false
	}
	var found *manifest.LinkShare
	for i := range shares {
		if shares[i].Binding == rec.Binding {
			found = &shares[i]
			break
		}
	}
	if found == nil {
		return "", "", false
	}
	b, err := binding.ReadNamed(rec.Binding)
	if err != nil || b.Mailbox() || filepath.Clean(b.Root) != filepath.Clean(root) {
		return "", "", false
	}
	// The binding record, the live endpoint session, and the request's
	// signed session must all name one native session.
	if b.Target != rec.TargetID || b.NativeSession != rec.NativeSession ||
		ep.NativeSessionID(b.Target) != rec.NativeSession {
		return "", "", false // not the request's live native session
	}
	for _, sess := range ep.Sessions() {
		if sess.TargetID == rec.TargetID {
			if sess.Epoch != rec.Epoch {
				return "", "", false // a newer generation owns the session now
			}
			return found.Approvals, found.Consent, true
		}
	}
	return "", "", false
}

// Select returns the interaction to publish for rec: the earliest opened
// fully eligible dialog (ties by id), or a re-render of the one rec pins.
func (s *linkDialogSource) Select(rec *requests.Record) *protocol.Interaction {
	dir, err := dialog.DirRead(rec.Binding)
	if err != nil {
		return nil
	}
	// The pinned path holds the same R1 baseline rule: without a captured
	// baseline, nothing is proven of the run and nothing publishes.
	if rec.DialogsBefore == nil {
		return nil // R1: no baseline was captured, so nothing is proven of the run
	}
	if rec.Interaction != nil && slices.Contains(rec.LinkDialogs, rec.Interaction.InteractionID) {
		// Re-render the pinned dialog only: its answer state may have
		// moved. A different dialog never steals the pin. The re-render
		// runs even when the live policy no longer permits an answer —
		// then takesAnswers lights the button off and the card stays as a
		// disabled copy with its published text (R5), instead of the old
		// live card surviving under a dead policy.
		in, eligible := s.renderDialog(dir, rec, rec.Interaction.InteractionID, true)
		if !eligible {
			return nil
		}
		return in
	}
	approvals, consent, ok := s.sharePolicy(rec)
	if !ok || !linkTakesAnswers(approvals, consent) {
		return nil
	}
	reqs, err := dialog.Requests(dir)
	if err != nil {
		return nil
	}
	// Earliest first, ties by id; render decides eligibility on the actual
	// request it read, so an earlier unrenderable dialog never hides a
	// later eligible one.
	sort.Slice(reqs, func(i, j int) bool {
		if !reqs[i].OpenedAt.Equal(reqs[j].OpenedAt) {
			return reqs[i].OpenedAt.Before(reqs[j].OpenedAt)
		}
		return reqs[i].ID < reqs[j].ID
	})
	for _, req := range reqs {
		if in, eligible := s.render(dir, rec, req.ID); eligible {
			return in
		}
	}
	return nil
}

// takesAnswers reports whether the record's share takes link answers now:
// the same judgment AnswerPolicy makes for the exact link, so the
// published remote_answer flag and the admission check can never diverge.
func (s *linkDialogSource) takesAnswers(rec *requests.Record) bool {
	approvals, consent, ok := s.sharePolicy(rec)
	return ok && linkTakesAnswers(approvals, consent)
}

// linkTakesAnswers reports whether a share of this approvals policy and
// consent takes answers over the link. Admission, the published
// remote_answer flag, and the attach summary all judge by this one rule:
// policy link and a consent that takes a signed grant.
func linkTakesAnswers(approvals, consent string) bool {
	return approvals == manifest.ApprovalsLink && linkio.TakesGrant(consent)
}

// render reads one dialog and builds its interaction when it is fully
// eligible: open, complete, not of the run's start baseline, (Codex) of the
// request's exact turn, of the binding's native session, offering a legal
// set of link choices — at most one allow-once, at most one deny-once, at
// least one of the two, distinct labels — and every rendered field within
// the /1 schema bounds.
func (s *linkDialogSource) render(dir string, rec *requests.Record, id string) (*protocol.Interaction, bool) {
	return s.renderDialog(dir, rec, id, false)
}

// renderDialog builds one dialog's interaction when it is eligible.
// pinned: the request already publishes this dialog, so a re-render also
// runs while its answer file exists — the card stays, with the button
// dark, until the resolution. A new selection only ever renders an open
// dialog.
func (s *linkDialogSource) renderDialog(dir string, rec *requests.Record, id string, pinned bool) (*protocol.Interaction, bool) {
	phase, req, _, resolved, err := dialog.Lookup(dir, id)
	if err != nil || resolved != nil || req.ID != id {
		return nil, false // resolved, gone, or unreadable: publish nothing
	}
	if phase != dialog.PhaseOpen && !pinned {
		return nil, false // an answered dialog is about to resolve
	}
	if !req.Complete || req.FreeText {
		return nil, false
	}
	if slices.Contains(rec.DialogsBefore, req.ID) {
		return nil, false // a dialog of the run's start baseline is not of the run
	}
	if req.Harness == "codex" {
		turn := ""
		if rec.NativeRun != nil {
			turn, _ = codex.TurnOfRun(*rec.NativeRun)
		}
		if req.Turn == "" || req.Turn != turn {
			return nil, false // Codex still requires the exact turn
		}
	}
	// The dialog's own session must be the request's signed session, the
	// immutable value the record carries — not a second read of the
	// binding file, which may have moved since sharePolicy checked it.
	if req.Session == "" || req.Session != rec.NativeSession {
		return nil, false // identity unavailable or not the signed session: withhold
	}
	// The render is the one the answer write re-checks the signature
	// against: dialog.RenderLinkInteraction is shared with the write so a
	// record whose text or options changed renders a different dialog and
	// the pinned digest check refuses the answer.
	in, ok := dialog.RenderLinkInteraction(req)
	if !ok {
		return nil, false
	}
	// remote_answer is true while the dialog has no answer file and
	// the share still takes link answers: the buttons are live. An
	// answer file — the link's or the owner's from Buzz — or a policy
	// that moved takes them dark in a new revision of the same card.
	in.RemoteAnswer = phase == dialog.PhaseOpen && s.takesAnswers(rec)
	return in, true
}

// Present reports the dialog's state at the retirement horizon: present
// when its request file is readable and no resolution replaced it; when the
// lookup sees a resolution, resolved carries it mapped.
func (s *linkDialogSource) Present(binding, id string) (bool, protocol.Resolution, bool) {
	dir, err := dialog.DirRead(binding)
	if err != nil {
		return false, protocol.Resolution{}, false
	}
	phase, _, _, resolved, err := dialog.Lookup(dir, id)
	if err != nil {
		return false, protocol.Resolution{}, false
	}
	if resolved != nil {
		return false, mapResolved(*resolved), true
	}
	// An answered-but-unresolved dialog still exists: its resolution may
	// land or be rejected, so it stays owed.
	if phase == dialog.PhaseOpen || phase == dialog.PhaseAnswered {
		return true, protocol.Resolution{}, false
	}
	return false, protocol.Resolution{}, false
}

// RequestLive reports whether the record still runs on the binding's live
// native session in the generation it was admitted in: the identity half
// of sharePolicy, without the share's approvals.
func (s *linkDialogSource) RequestLive(rec *requests.Record) bool {
	_, _, ok := s.sharePolicy(rec)
	return ok
}

// HasAnswer reports whether the binding's dialog already holds a recorded
// answer or a resolution for the id.
func (s *linkDialogSource) HasAnswer(bindingName, id string) bool {
	dir, err := dialog.DirRead(bindingName)
	if err != nil {
		return false
	}
	phase, _, _, resolved, err := dialog.Lookup(dir, id)
	if err != nil {
		return false
	}
	return phase == dialog.PhaseAnswered || phase == dialog.PhaseResolved || resolved != nil
}

// AnswerPolicy reports whether the host's share of the binding takes link
// answers now, and the reason when it does not: approvals_terminal (no
// running link under the host shares the binding, or its approvals are
// not link), consent_local (its consent is not passkey),
// harness_unsupported (the harness behind the binding cannot apply a
// link answer), in that order.
func (s *linkDialogSource) AnswerPolicy(host, bindingName string) (bool, string) {
	approvals, consent, ok := s.linkSharePolicy(host, bindingName)
	if !ok || approvals != manifest.ApprovalsLink {
		return false, "approvals_terminal"
	}
	if !linkio.TakesGrant(consent) {
		return false, "consent_local"
	}
	b, err := binding.ReadNamed(bindingName)
	if err != nil {
		return false, "approvals_terminal"
	}
	switch b.Harness {
	case "claude", "codex":
		return true, ""
	default:
		// The narrow start of the ratified plan: only the harnesses whose
		// seam reads an answer file and writes the resolution with its by.
		// ACP joins when its seam marks dialogs complete; pi never has.
		return false, "harness_unsupported"
	}
}

// linkSharePolicy resolves the running link under the creator host and
// the share naming the binding: the share list and the binding's live
// native session, asked again at answer time. It carries no request, so
// it compares the binding's own epoch, not a record's.
func (s *linkDialogSource) linkSharePolicy(host, bindingName string) (approvals, consent string, ok bool) {
	s.links.mu.Lock()
	var shares []manifest.LinkShare
	for _, r := range s.links.running {
		if r.carrier.Host() == host {
			shares = r.link.Shares
			break
		}
	}
	ep, root := s.links.ep, s.links.root
	s.links.mu.Unlock()
	var found *manifest.LinkShare
	for i := range shares {
		if shares[i].Binding == bindingName {
			found = &shares[i]
			break
		}
	}
	if found == nil || ep == nil {
		return "", "", false
	}
	b, err := binding.ReadNamed(bindingName)
	if err != nil || b.Mailbox() || filepath.Clean(b.Root) != filepath.Clean(root) {
		return "", "", false
	}
	for _, sess := range ep.Sessions() {
		if sess.TargetID == b.Target {
			return found.Approvals, found.Consent, true
		}
	}
	return "", "", false
}

// WriteAnswer records the verified answer in the binding's dialog
// directory. The policy judgment is the seam's; the write itself holds
// the pinned handles and judges the record it publishes through: the
// signature is checked against a re-render of the record read through
// the held dialog directory (dialog_changed when the dialog moved since
// publication), the option is mapped from that same pinned request, and
// the answer file is created exclusively and synced through the pinned
// fsq sync. It creates no directory — a missing one is already_resolved,
// the same refusal a resolved or answered dialog gets.
func (s *linkDialogSource) WriteAnswer(host, bindingName, id, label, role, interactionDigest, documentDigest, credential string) (protocol.Code, string) {
	if _, _, ok := s.linkSharePolicy(host, bindingName); !ok {
		return protocol.CodeUnshared, "the binding is not shared with this link now"
	}
	dir, err := dialog.DirRead(bindingName)
	if err != nil {
		return protocol.CodeAlreadyResolved, "the binding's dialog directory is gone"
	}
	switch err := dialog.WriteLinkAnswer(dir, id, label, role, interactionDigest, documentDigest, credential); {
	case err == nil:
		return "", ""
	case errors.Is(err, dialog.ErrLinkDialogChanged):
		return protocol.CodeDialogChanged, err.Error()
	case errors.Is(err, dialog.ErrLinkDialogGone), errors.Is(err, dialog.ErrLinkAnswered):
		return protocol.CodeAlreadyResolved, err.Error()
	case errors.Is(err, dialog.ErrLinkNoSuchChoice):
		return protocol.CodeInvalid, err.Error()
	default:
		return protocol.CodeInternal, err.Error()
	}
}

// Outcome reads how a published dialog ended, in the ratified order: the
// resolution file, then the dialog history entry, then not done — the owed
// horizon later records unknown. A missing file is never read as a winner.
func (s *linkDialogSource) Outcome(binding, id string) (protocol.Resolution, bool) {
	dir, err := dialog.DirRead(binding)
	if err != nil {
		return protocol.Resolution{}, false
	}
	if r, ok, _ := dialog.ReadResolved(dir, id); ok {
		return mapResolved(r), true
	}
	// A missing directory is no files: the history fallback decides.
	entries, err := dialog.History(time.Time{})
	if err != nil {
		return protocol.Resolution{}, false
	}
	for _, e := range entries {
		if e.ID != id || e.Binding != binding {
			continue
		}
		return mapHistoryEntry(e), true
	}
	return protocol.Resolution{}, false
}

// mapResolved maps a dialog resolution file onto the protocol outcome.
func mapResolved(r dialog.Resolved) protocol.Resolution {
	out := protocol.Resolution{InteractionID: r.ID, Option: r.Label}
	switch {
	case r.By == dialog.ByLink:
		out.Outcome = protocol.ResolutionAnswered
	case r.By == dialog.ByBuzz || r.By == dialog.ByTerminal:
		out.Outcome = protocol.ResolutionElsewhere
	case r.Outcome == dialog.OutcomeAllowed || r.Outcome == dialog.OutcomeDenied || r.Outcome == dialog.OutcomeAnswered:
		// A resolution with a taken choice but an unrecognized closer
		// still names who has the answer: not ours to claim.
		out.Outcome = protocol.ResolutionElsewhere
	default:
		out.Outcome = protocol.ResolutionRunEnded
		out.Option = ""
	}
	return out
}

// mapHistoryEntry maps a dialog history entry onto the protocol outcome; it
// is what a link reads when the dialog's files are gone (R2).
func mapHistoryEntry(e dialog.HistoryEntry) protocol.Resolution {
	out := protocol.Resolution{InteractionID: e.ID, Option: e.Label}
	switch {
	case e.By == dialog.ByLink:
		out.Outcome = protocol.ResolutionAnswered
	case e.By == dialog.ByBuzz || e.By == dialog.ByTerminal:
		out.Outcome = protocol.ResolutionElsewhere
	case e.Outcome == dialog.OutcomeAllowed || e.Outcome == dialog.OutcomeDenied || e.Outcome == dialog.OutcomeAnswered:
		out.Outcome = protocol.ResolutionElsewhere
	default:
		out.Outcome = protocol.ResolutionRunEnded
		out.Option = ""
	}
	return out
}

// Schema bounds for a published interaction (remote-request-v1).
const ()
