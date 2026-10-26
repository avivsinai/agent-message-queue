package core

import (
	"maps"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

func (e *Endpoint) get(cmd *protocol.Command, src Source) (protocol.Reply, error) {
	host, targetID, requestID, err := protocol.DecodeRef(cmd.RequestRef)
	if err != nil {
		return protocol.Reply{}, err
	}
	if outsideNamespace(src, host) {
		return protocol.Reply{}, noRecord()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok, err := e.store.Get(requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID})
	if err != nil {
		return protocol.Reply{}, err
	}
	if !ok {
		return protocol.Reply{}, noRecord()
	}
	if !readableBy(src, rec) {
		return protocol.Reply{}, notOwned(src, rec)
	}
	if snap, ok := e.failedLocked(rec); ok {
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestGet}}, nil
	}
	out := protocol.Outcome{Op: protocol.OpRequestGet}
	if rec.State == protocol.StateRejected {
		out.Message = rec.RefusalReason
	}
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: out}, nil
}

func (e *Endpoint) cancel(cmd *protocol.Command, src Source) (protocol.Reply, error) {
	host, targetID, requestID, err := protocol.DecodeRef(cmd.RequestRef)
	if err != nil {
		return protocol.Reply{}, err
	}
	if targetID != cmd.TargetID {
		return protocol.Reply{}, protocol.Refuse(protocol.CodeInvalid, "request_ref names a different target")
	}
	key := requests.Key{CreatorHost: host, TargetID: targetID, RequestID: requestID}
	now := protocol.FormatTime(e.now())
	if outsideNamespace(src, host) {
		return protocol.Reply{}, noRecord()
	}

	e.mu.Lock()
	rec, exists, err := e.store.Get(key)
	if err != nil {
		e.mu.Unlock()
		return protocol.Reply{}, err
	}
	if !exists {
		// Cancel arrived before its submit: leave a tombstone that a later
		// submit cannot execute past. Only the Buzz carrier creates buzz-
		// hosts, so a tombstone on one must come from that same share.
		// The match ignores case: on a case-insensitive filesystem a BUZZ-
		// tombstone is the same file.
		if strings.HasPrefix(strings.ToLower(host), buzzHostPrefix) && (src.Origin["carrier"] != "buzz" || src.Host != host) {
			e.mu.Unlock()
			return protocol.Reply{}, protocol.Refuse(protocol.CodeUnshared, "only the owner's Buzz share can cancel its requests")
		}
		// A link creates no tombstones: it cancels only after its submit's
		// outcome, and its commands for one request run in order, so an
		// absent request is one it never sent. Nobody else may plant one in
		// a link's namespace either.
		if isLink(src) {
			e.mu.Unlock()
			return protocol.Reply{}, noRecord()
		}
		if strings.HasPrefix(strings.ToLower(host), linkHostPrefix) {
			e.mu.Unlock()
			return protocol.Reply{}, protocol.Refuse(protocol.CodeUnshared, "only the link that submitted a request can cancel it")
		}
		rec = &requests.Record{
			Snapshot: protocol.Snapshot{
				Schema:      protocol.SchemaRequest,
				RequestID:   requestID,
				CreatorHost: host,
				TargetID:    targetID,
				Epoch:       cmd.Epoch,
				Revision:    1,
				State:       protocol.StateCancelled,
				Code:        protocol.CodeCancelledBeforeAdmission,
				Cancel:      &protocol.Cancel{RequestedAt: now, Disposition: protocol.CancelConfirmed},
				ObservedAt:  now,
			},
			Tombstone: true,
			Origin:    maps.Clone(src.Origin),
		}
		err := e.store.Create(rec)
		if err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		e.notifyLocked(rec)
		e.mu.Unlock()
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel}}, nil
	}
	if !ownedBy(src, rec) {
		e.mu.Unlock()
		return protocol.Reply{}, notOwned(src, rec)
	}
	// Validate the cancel command against the original request binding rather
	// than silently substituting the record's trusted values. A
	// cancel naming a different epoch is for a generation that no longer
	// exists; it must not interrupt the current run.
	if cmd.Epoch != rec.Epoch {
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Code: protocol.CodeStaleEpoch}}, nil
	}
	if exp, perr := protocol.ParseTime(cmd.NotAfter); perr == nil && e.now().After(exp) {
		// The cancel command's own admission window has closed; do not act on
		// a stale interrupt.
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Code: protocol.CodeExpired}}, nil
	}
	if rec.State.Terminal() && !readmittableBusy(rec) {
		e.mu.Unlock()
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Disposition: protocol.CancelNoopTerminal}}, nil
	}
	if rec.State == protocol.StateReceived || readmittableBusy(rec) {
		// Nothing was sent natively: the cancel is final at once, and a
		// later identical resubmit returns the cancelled record.
		e.transitionLocked(rec, causeCancelledBeforeAdmission, nativeEvidence{})
		if _, err := e.commitLocked(rec, e.targets[targetID]); err != nil {
			e.mu.Unlock()
			return protocol.Reply{}, err
		}
		snap := rec.Snapshot
		e.mu.Unlock()
		e.publishRevision(rec)
		return protocol.Reply{Snapshot: snap, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Disposition: protocol.CancelConfirmed}}, nil
	}
	t := e.targets[targetID]
	e.mu.Unlock()

	var ev CancelEvidence
	var nerr error
	if t == nil {
		ev = CancelEvidence{Disposition: protocol.CancelRequested, Message: "attachment offline; intent recorded"}
	} else {
		ev, nerr = t.att.CancelExact(key, rec.Epoch)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	rec, _, err = e.store.Get(key)
	if err != nil {
		return protocol.Reply{}, err
	}
	if readmittableBusy(rec) {
		// Submit answered busy with nothing sent while the lock was free:
		// the cancel ends the tombstone, as it does before a dispatch.
		e.transitionLocked(rec, causeCancelledBeforeAdmission, nativeEvidence{})
		if _, err := e.commitLocked(rec, t); err != nil {
			return protocol.Reply{}, err
		}
		e.publishLocked(rec)
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Disposition: protocol.CancelConfirmed}}, nil
	}
	if rec.State.Terminal() {
		return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel, Disposition: protocol.CancelNoopTerminal}}, nil
	}
	if nerr != nil {
		ev = CancelEvidence{Disposition: protocol.CancelRequested, Message: nerr.Error()}
	}
	if ev.Disposition == protocol.CancelConfirmed {
		e.transitionLocked(rec, causeCancelledByRequest, nativeEvidence{})
	} else {
		// cancel_requested (intent recorded, run not stopped) or noop_terminal
		// (run already done, nothing to stop). Keep the disposition without
		// going terminal; reconcile re-drives if a run is bound.
		// Guard against downgrading a confirmed cancel.
		if rec.Cancel == nil {
			rec.Cancel = &protocol.Cancel{RequestedAt: now}
		}
		if rec.Cancel.Disposition != protocol.CancelConfirmed {
			rec.Cancel.Disposition = ev.Disposition
		}
	}
	if _, err := e.commitLocked(rec, t); err != nil {
		return protocol.Reply{}, err
	}
	e.publishLocked(rec)
	return protocol.Reply{Snapshot: rec.Snapshot, Outcome: protocol.Outcome{Op: protocol.OpRequestCancel}}, nil
}

// buzzHostPrefix starts every creator host the Buzz carrier derives.
const buzzHostPrefix = "buzz-"

// fromOwnerShare reports whether src is the Buzz share that submitted rec.
// An answer to an interaction and a cancel of a Buzz-submitted request are
// the owner's decisions, so only the owner's DM on the share whose body and
// channel submitted the request may
// make them. The AMQ mailbox carrier stamps its own origin and the local IPC
// socket stamps none, so a local sender, the asking agent included, can
// never pass.
func fromOwnerShare(src Source, rec *requests.Record) bool {
	return sameShare(src, rec.CreatorHost, rec.Origin)
}

// sameShare reports whether src is the Buzz share with creator host host
// and routing origin.
func sameShare(src Source, host string, origin map[string]string) bool {
	o, r := src.Origin, origin
	// The creator host is derived from relay, body and owner, so it also
	// catches a new owner on the same body and channel.
	return src.Host == host && o["carrier"] == "buzz" && r["carrier"] == "buzz" &&
		o["body"] != "" && o["body"] == r["body"] && o["channel"] == r["channel"]
}

// isLink reports whether src arrived over a link carrier.
func isLink(src Source) bool { return src.Origin["carrier"] == CarrierLink }

// outsideNamespace reports whether a link source names a request another
// source created. It is checked before any lookup, so a link learns nothing
// about request ids outside its own creator host: it hears not_found, as for
// an absent request.
func outsideNamespace(src Source, refHost string) bool {
	return isLink(src) && refHost != src.Host
}

// isLocal reports whether src is the local socket: the user's own terminal.
func isLocal(src Source) bool { return src.Host == LocalHost && len(src.Origin) == 0 }

// readableBy is the read rule (get, wait). The local socket, the user's own
// terminal, reads every record. A Buzz share reads only the records it
// submitted, and a link only the records it created. Other carriers (the AMQ
// mailbox) read every record but a link's.
func readableBy(src Source, rec *requests.Record) bool {
	switch {
	case isLink(src):
		return rec.Origin["carrier"] == CarrierLink && rec.CreatorHost == src.Host
	case src.Origin["carrier"] == "buzz":
		return fromOwnerShare(src, rec)
	case isLocal(src):
		return true
	default:
		return rec.Origin["carrier"] != CarrierLink
	}
}

// ownedBy is the cancel rule. A link record is cancelled only by the link
// that created it, and a link cancels nothing else. A Buzz record is
// cancelled only from the owner's share that submitted it.
// Other records keep the local rule: local senders carry no authenticated
// identity.
func ownedBy(src Source, rec *requests.Record) bool {
	switch {
	case isLink(src):
		return rec.Origin["carrier"] == CarrierLink && rec.CreatorHost == src.Host
	case rec.Origin["carrier"] == CarrierLink:
		return false
	case rec.Origin["carrier"] == "buzz":
		return fromOwnerShare(src, rec)
	default:
		return true
	}
}

// noRecord is the refusal for an absent request, and for a link naming a
// request it does not own: the two are indistinguishable to the link.
func noRecord() error {
	return protocol.Refuse(protocol.CodeNotFound, "no record for request_ref")
}

// notOwned is the refusal when readableBy or ownedBy fails.
func notOwned(src Source, rec *requests.Record) error {
	switch {
	case isLink(src):
		return noRecord()
	case src.Origin["carrier"] == "buzz":
		return protocol.Refuse(protocol.CodeUnshared, "a Buzz share reaches only the requests it submitted")
	case rec.Origin["carrier"] == CarrierLink:
		return protocol.Refuse(protocol.CodeUnshared, "only the link that submitted this request can read or cancel it")
	default:
		return protocol.Refuse(protocol.CodeUnshared, "only the owner's Buzz share can cancel its requests")
	}
}

// localSubmitted reports whether src is the local socket (LocalHost, no
// carrier origin) and rec is a request that socket submitted.
func localSubmitted(src Source, rec *requests.Record) bool {
	return src.Host == LocalHost && len(src.Origin) == 0 && rec.CreatorHost == LocalHost && len(rec.Origin) == 0
}

// localAnswer reports whether src may answer cmd although it is no owner
// share: the Buzz Desktop agent (amq-acp) relays the owner's answer over the
// local socket, which has no owner share. It holds only for a local request
// answered from the local socket, while t is the Claude attachment the
// request runs on and its pending interaction is exactly cmd.InteractionID,
// a Claude approval that takes a remote answer. The option is that
// approval's reject option, or its approve option when the approval offers
// allow with proof and cmd carries evidence. The endpoint verifies nothing
// here: the attachment and then the hook check the evidence against the
// hook's own pin, and nothing else allows. The caller reads rec and t
// together under e.mu.
func localAnswer(src Source, rec *requests.Record, t *target, cmd *protocol.Command) bool {
	if !localSubmitted(src, rec) || t == nil {
		return false
	}
	in := rec.Interaction
	if in == nil || in.InteractionID != cmd.InteractionID || in.Kind != "approval" || !in.RemoteAnswer ||
		in.RejectOption == "" || in.RejectOption == in.ApproveOption {
		return false
	}
	deny := cmd.Option == in.RejectOption
	allow := in.ApproveOption != "" && in.ApproveProof && cmd.Option == in.ApproveOption && len(cmd.Evidence) > 0
	if !deny && !allow {
		return false
	}
	s := t.att.Inspect()
	return s.Harness == "claude_code" && s.Epoch == rec.Epoch
}
