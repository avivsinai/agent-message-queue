package core

import (
	"slices"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// LinkDialogs is the machine's view of a binding's dialog directory: which
// open dialog of a running link request to publish as its interaction, and
// how a published dialog ended. The endpoint owns the record and the
// revision; the implementation owns the files and the share policy.
//
// Both members read the filesystem and run OUTSIDE the endpoint lock, like
// the attachment Lookup they sit beside.
type LinkDialogs interface {
	// Select returns the interaction to publish for a running link request,
	// or nil. When the record pins a dialog this endpoint selected, Select
	// re-renders that dialog (its answer state may have moved); otherwise it
	// chooses the earliest opened eligible open dialog, ties by id.
	// Eligibility, the share's approvals policy and its consent are the
	// implementation's judgment; a nil result publishes nothing.
	Select(rec *requests.Record) *protocol.Interaction

	// Outcome reads how a published dialog ended, in the ratified order:
	// its resolution file, else the binding's dialog history entry, else not
	// done — the owed-outcome horizon later records unknown. done resolves
	// the pinned interaction with the returned outcome.
	Outcome(binding, id string) (resolution protocol.Resolution, done bool)

	// Present reports the dialog's state at the retirement horizon.
	// present: the request file exists and no resolution replaced it — the
	// outcome may still arrive, so the horizon keeps the id owed. When the
	// lookup instead sees a resolution — one that landed after the
	// caller's outcome ask — resolved carries it, so the caller settles
	// that outcome instead of retiring it as unknown.
	Present(binding, id string) (present bool, res protocol.Resolution, resolved bool)

	// AnswerPolicy reports whether the binding's share takes link answers
	// for the authenticated host now, and the reason when it does not:
	// approvals_terminal, consent_local or harness_unsupported, in that
	// order when more than one holds. Two links may share one binding with
	// different policies, so the host is part of the question.
	AnswerPolicy(host, binding string) (available bool, reason string)

	// RequestLive reports whether a running record still runs on the
	// binding's live native session, in the generation it was admitted
	// in — the identity judgment publication made, repeated at answer
	// time.
	RequestLive(rec *requests.Record) bool

	// WriteAnswer records a verified link answer in the binding's dialog
	// directory: the answers file created exclusively, the file and its
	// directory synced. "", "" means durable. Any other return is the
	// refusal code and its reason; nothing was recorded, except for
	// internal, where durability could not be confirmed.
	WriteAnswer(host, binding, id, label, documentDigest, credential string) (protocol.Code, string)
}

// SetLinkDialogs installs the link-dialog seam. Until it is set, no link
// dialog is selected, published or resolved.
func (e *Endpoint) SetLinkDialogs(l LinkDialogs) {
	e.mu.Lock()
	e.linkDialogs = l
	e.mu.Unlock()
}

// isLinkDialog reports whether id is an interaction this endpoint selected
// from a binding's dialog directory.
func isLinkDialog(rec *requests.Record, id string) bool {
	return slices.Contains(rec.LinkDialogs, id)
}

// reconcileLinkDialog selects, refreshes and resolves the link dialog of one
// link request. It reads the dialog directory without the lock and commits
// under it, and it never changes the record's state: the disposition of a
// cleared link interaction travels the existing owed-outcome mechanism,
// across run end and restart. New selections and re-renders are a running
// record's business; an already-published dialog also settles from its
// files while the record is uncertain — native liveness is unknown, but the
// disposition lives on the machine, not in the attachment.
func (e *Endpoint) reconcileLinkDialog(rec *requests.Record) error {
	if e.linkDialogs == nil || rec.Binding == "" {
		return nil
	}
	publish := rec.State == protocol.StateRunning
	if !publish && rec.State != protocol.StateUncertain {
		return nil
	}
	key := keyOfRecord(rec)

	// Unlocked file reads, against the record as it was fetched. Both are
	// re-checked against the fresh record under the lock.
	sel := (*protocol.Interaction)(nil)
	if publish {
		sel = e.linkDialogs.Select(rec)
	}
	out := (*protocol.Resolution)(nil)
	if rec.Interaction != nil && isLinkDialog(rec, rec.Interaction.InteractionID) {
		if r, done := e.linkDialogs.Outcome(rec.Binding, rec.Interaction.InteractionID); done {
			out = &r
		}
	}
	if sel == nil && out == nil {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	cur, exists, err := e.store.Get(key)
	if err != nil || !exists {
		return err
	}
	if cur.Binding == "" {
		return nil
	}
	changed := false
	if out != nil && cur.Interaction != nil && cur.Interaction.InteractionID == out.InteractionID {
		if cur.State == protocol.StateRunning || cur.State == protocol.StateUncertain {
			e.transitionLocked(cur, causeNone, nativeEvidence{
				clearInteraction: true,
				resolvedRemotely: out.Outcome == protocol.ResolutionAnswered,
				outcome:          out.Outcome,
				option:           out.Option,
			})
			changed = true
		}
	}
	if sel != nil && cur.State == protocol.StateRunning && !resolvedIn(cur.Resolved, sel.InteractionID) {
		switch {
		case cur.Interaction == nil:
			// A new selection, never one another running link request
			// already pins.
			if !e.linkDialogPinnedByOther(cur, sel.InteractionID) {
				if !isLinkDialog(cur, sel.InteractionID) {
					cur.LinkDialogs = append(cur.LinkDialogs, sel.InteractionID)
				}
				cur.Interaction = sel
				changed = true
			}
		case cur.Interaction.InteractionID == sel.InteractionID && !sameInteraction(*cur.Interaction, *sel):
			// The pinned dialog's answer state moved (an answer file
			// appeared, or was removed as invalid): republish the same
			// interaction with the refreshed RemoteAnswer.
			cp := *sel
			cur.Interaction = &cp
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if _, err := e.commitLocked(cur, nil); err != nil {
		e.notifyStorageFailureLocked(cur, err)
		return err
	}
	return nil
}

// linkDialogPinnedByOther reports whether another running link record
// already pins id as its interaction.
func (e *Endpoint) linkDialogPinnedByOther(rec *requests.Record, id string) bool {
	recs := e.store.ListSweep()
	self := keyOfRecord(rec)
	for _, other := range recs {
		if keyOfRecord(other) == self || other.State != protocol.StateRunning || other.Interaction == nil {
			continue
		}
		if other.Interaction.InteractionID == id {
			return true
		}
	}
	return false
}
