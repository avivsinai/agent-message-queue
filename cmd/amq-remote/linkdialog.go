package main

import (
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
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
	approvals, consent, ok := s.sharePolicy(rec)
	if !ok || approvals != manifest.ApprovalsLink || !linkio.TakesGrant(consent) {
		return nil
	}
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
		// moved. A different dialog never steals the pin.
		in, eligible := s.render(dir, rec, rec.Interaction.InteractionID)
		if !eligible {
			return nil
		}
		return in
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

// render reads one dialog and builds its interaction when it is fully
// eligible: open, complete, not of the run's start baseline, (Codex) of the
// request's exact turn, of the binding's native session, offering a legal
// set of link choices — at most one allow-once, at most one deny-once, at
// least one of the two, distinct labels — and every rendered field within
// the /1 schema bounds.
func (s *linkDialogSource) render(dir string, rec *requests.Record, id string) (*protocol.Interaction, bool) {
	phase, req, _, resolved, err := dialog.Lookup(dir, id)
	if err != nil || resolved != nil || req.ID != id {
		return nil, false // resolved, gone, or unreadable: publish nothing
	}
	if phase != dialog.PhaseOpen {
		return nil, false // an answered dialog is about to resolve
	}
	if !req.Complete || req.FreeText {
		return nil, false
	}
	if slices.Contains(rec.DialogsBefore, req.ID) {
		return nil, false // a dialog of the run's start baseline is not of the run
	}
	if req.Harness == "codex" && (req.Turn == "" || rec.NativeRun == nil || req.Turn != *rec.NativeRun) {
		return nil, false // Codex still requires the exact turn
	}
	// The dialog's own session must be the request's signed session, the
	// immutable value the record carries — not a second read of the
	// binding file, which may have moved since sharePolicy checked it.
	if req.Session == "" || req.Session != rec.NativeSession {
		return nil, false // identity unavailable or not the signed session: withhold
	}
	var (
		allow, deny dialog.Option
		haveAllow   bool
		haveDeny    bool
	)
	for _, o := range req.Options {
		if o.Scope != dialog.ScopeOnce {
			continue
		}
		if o.Deny {
			if haveDeny {
				return nil, false
			}
			deny, haveDeny = o, true
			continue
		}
		if haveAllow {
			return nil, false
		}
		allow, haveAllow = o, true
	}
	if !haveAllow && !haveDeny {
		return nil, false // nothing the link may choose once
	}
	if haveAllow && haveDeny && allow.Label == deny.Label {
		return nil, false // roles must stay distinguishable
	}
	prompt := req.Tool + "\n" + req.Text
	if len(prompt) > maxLinkPromptBytes || len(id) > 128 {
		return nil, false
	}
	in := &protocol.Interaction{
		InteractionID: req.ID,
		Kind:          "approval",
		Prompt:        prompt,
		Options:       []string{},
		// Unit A publishes display-only: the answer seam that takes the
		// link's choice ships later, so remote_answer stays absent until
		// then and no server offers an answer AMQ cannot take.
		RemoteAnswer: false,
	}
	for _, o := range req.Options {
		if (o.Deny && haveDeny && o == deny) || (!o.Deny && haveAllow && o == allow) {
			if o.Label == "" || len(o.Label) > maxLinkOptionBytes {
				return nil, false
			}
			in.Options = append(in.Options, o.Label)
		}
	}
	if haveAllow {
		in.ApproveOption = allow.Label
	}
	if haveDeny {
		in.RejectOption = deny.Label
	}
	if len(in.Options) == 0 {
		return nil, false
	}
	return in, true
}

// Present reports whether the dialog still exists on the machine: its
// request file is readable and no resolution replaced it.
func (s *linkDialogSource) Present(binding, id string) bool {
	dir, err := dialog.DirRead(binding)
	if err != nil {
		return false
	}
	phase, _, _, resolved, err := dialog.Lookup(dir, id)
	// An answered-but-unresolved dialog still exists: its resolution may
	// land or be rejected, so it stays owed.
	return err == nil && (phase == dialog.PhaseOpen || phase == dialog.PhaseAnswered) && resolved == nil
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
const (
	maxLinkPromptBytes = 16384
	maxLinkOptionBytes = 256
)
