// Package amit is the Amit (pi) adapter factory. It registers the
// kind:"amit" factory under the named-factory registry so a manifest entry
// `{"kind":"amit","target":"amit","config":{"handle":"<session handle>"}}`
// builds an AmitAttachment without editing serve.
//
// The adapter implements the amit-remote contract v1 (packages/amit/
// extensions/amit-remote/CONTRACT.md, merged to amit master as 1e21930a).
// It is extension-only: it does not spawn, own, or supervise the Amit
// process (ADR invariant 1 — up supervises serve only; serve never owns a
// harness). All coordination happens through the contract's file seam under
// <AM_ROOT>/agents/<handle>/extensions/amit-remote/:
//
//	requests/<ref>.json   written by THIS adapter (atomic, create-new)
//	receipts/<ref>.json   read by THIS adapter (admission proof)
//	events/<ref>.jsonl    read by THIS adapter (terminal evidence)
//	bridge.liveness       read by THIS adapter (heartbeat freshness)
//
// Ownership (contract §8): the adapter writes ONLY requests/<ref>.json and
// reads ONLY receipts/, events/, and bridge.liveness. It never touches pi
// internals, the doorbell layer, or any other extension directory.
//
// Evidence: pi's sendUserMessage returns void and swallows rejections, and
// v1 has no native admission primitive — the per-ref RECEIPT is the only
// admission proof (contract §2). Until a receipt is observed the adapter
// publishes the sentinel epoch `unpinned` (§4/A4); the first receipt pins
// the session generation. A key the adapter retains nothing about is
// EvidenceUnknown, never EvidenceNone.
package amit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// config is the adapter-specific config block for an amit manifest entry.
type config struct {
	// Handle is the AMQ session handle whose amit-remote extension
	// directory this adapter coordinates with. The (root, handle) pair must
	// match the session identity entry the Amit-side extension resolved
	// (contract §Identity: identity entry is the source of truth, no env
	// fallback). The manifest is AMQ-owned (contract §8), so the handle is
	// stated here explicitly — never derived from AM_ME/AM_ROOT env.
	Handle string `json:"handle"`
}

// run is one bound request tracked by the adapter. The durable record is
// the extension's (receipts + events); this struct is the in-memory
// correlation the adapter rebuilds from those files on attach (§5).
type run struct {
	key     requests.Key
	ref     string
	runID   string
	epoch   string // epoch the submit was bound under; "" = recovered history (any epoch matches)
	state   protocol.State
	refused protocol.Code // set by a definitive refused event (§3), mapped per §6/A3

	text      strings.Builder
	errText   string
	nativeRef string

	confirmed bool  // receipt observed — admission proven (contract §2)
	terminal  bool  // first terminal event consumed (first-terminal-wins, §3)
	acked     bool  // endpoint acknowledged the retained result
	notFound  error // last seam error that blocks evidence (never guessed around)
}

// Attachment implements core.Attachment over the amit-remote file seam. It
// never touches the local editor and owns no process.
type Attachment struct {
	mu     sync.Mutex
	target string
	handle string
	dir    bridgeDir
	// epoch is the receipt-pinned session generation (§4). Empty means "no
	// receipt observed yet" — Inspect publishes the sentinel `unpinned` and
	// submits carry an empty epoch_hint (first contact, no check).
	epoch string

	runs      map[requests.Key]*run
	order     []requests.Key // bind order, for bounded pruning + deterministic consume
	listeners map[int]func(core.NativeEvent)
	nextID    int
	now       func() time.Time
	// submitWait bounds the post-publication receipt poll in Submit. The
	// extension writes the receipt when it delivers (§2); a poll window
	// that expires with a live bridge leaves the record UNCERTAIN, never
	// rejected (AMQ never replays a ref, §2).
	submitWait time.Duration
}

// maxRetained bounds the retained run map; terminal+acked runs are dropped
// FIFO. Process-local correlation state, bounded so a long-lived attachment
// cannot grow it without limit.
const maxRetained = 256

// defaultSubmitWait is the production receipt-poll window.
const defaultSubmitWait = 2 * time.Second

// submitPollStep is the poll cadence inside the receipt window.
const submitPollStep = 25 * time.Millisecond

// New builds an attachment over the contract's file seam for the manifest
// target. The epoch is never an argument: it is OBSERVED from receipts
// (§4) — until the first receipt Inspect publishes the `unpinned` sentinel.
func New(target, handle string, dir bridgeDir) (*Attachment, error) {
	if target == "" {
		return nil, fmt.Errorf("amit: target is required")
	}
	if handle == "" {
		return nil, fmt.Errorf("amit: handle is required")
	}
	a := &Attachment{
		target:     target,
		handle:     handle,
		dir:        dir,
		runs:       map[requests.Key]*run{},
		listeners:  map[int]func(core.NativeEvent){},
		now:        time.Now,
		submitWait: defaultSubmitWait,
	}
	// §5 restart recovery: rebuild in-memory correlation from the durable
	// seam BEFORE any endpoint call. A recovered attachment never
	// redispatches: an existing request file plus any receipt/event answers
	// from history (§5).
	a.recoverLocked()
	return a, nil
}

// Factory builds an AmitAttachment from a registry.FactoryConfig.
func Factory(_ context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	var c config
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &c); err != nil {
			return nil, fmt.Errorf("parse amit config: %w", err)
		}
	}
	if c.Handle == "" {
		return nil, fmt.Errorf("amit config: handle is required (the amit-remote session handle)")
	}
	if err := fsq.ValidateHandle(c.Handle); err != nil {
		return nil, fmt.Errorf("amit config: invalid handle: %v", err)
	}
	dir := bridgeDir{dir: bridgePath(cfg.Root, c.Handle)}
	if st, err := os.Stat(dir.dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("amit: amit-remote extension directory not found at %s (is the amit-remote extension running for handle %q?)", dir.dir, c.Handle)
	}
	return New(cfg.Target, c.Handle, dir)
}

func init() {
	registry.Register("amit", Factory)
}

// clientRef is the request identity: protocol.EncodeRef of the record key,
// exactly as the contract's §Identity defines it. The extension keys its
// receipts and events by the same ref.
func clientRef(key requests.Key) string {
	return protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
}

// recoverLocked rebuilds runs from receipts/ + events/ (§5). Receipts are
// iterated oldest-first so the pinned epoch ends at the newest receipt's
// generation (the freshest receipt-proven observation). Junk files
// (unparseable, foreign protocol, undecodable ref) are skipped, never
// guessed into evidence.
func (a *Attachment) recoverLocked() {
	for _, rc := range a.dir.listReceipts() {
		creatorHost, targetID, requestID, derr := protocol.DecodeRef(rc.Ref)
		if derr != nil {
			continue
		}
		key := requests.Key{CreatorHost: creatorHost, TargetID: targetID, RequestID: requestID}
		if _, ok := a.runs[key]; ok {
			continue
		}
		r := a.bindRunLocked(key, "") // recovered history: epoch wildcard
		r.confirmed = true
		a.observeGenerationLocked(rc.SessionGeneration)
		a.applyEventsLocked(r)
	}
}

// lateBindLocked runs the recovery scan for one unseen key: a receipt for a
// ref whose run is not yet in the map (published by a prior process, or
// seeded/published between calls) binds a wildcard-epoch run with the
// receipt-proven state. Called under a.mu from the read paths before they
// decide "not found", so history never hides behind an in-memory miss (§5).
func (a *Attachment) lateBindLocked(key requests.Key) {
	if _, ok := a.runs[key]; ok {
		return
	}
	rc, err := a.dir.readReceipt(clientRef(key))
	switch {
	case err == nil && rc != nil:
		r := a.bindRunLocked(key, "") // recovered history: epoch wildcard
		r.confirmed = true
		a.observeGenerationLocked(rc.SessionGeneration)
		a.applyEventsLocked(r)
	case err != nil:
		// Unreadable or foreign-protocol receipt (§9): bind unconfirmed with
		// the seam error so Lookup surfaces it instead of guessing it into
		// evidence — or silently ignoring a record the endpoint still holds.
		r := a.bindRunLocked(key, "")
		r.notFound = err
	}
	// Absent receipt (nil, nil): leave unbound; Lookup answers unknown.
}

// bindRunLocked registers one correlation slot.
func (a *Attachment) bindRunLocked(key requests.Key, epoch string) *run {
	ref := clientRef(key)
	r := &run{
		key:       key,
		ref:       ref,
		runID:     "amit:" + ref,
		epoch:     epoch,
		state:     protocol.StateRunning,
		nativeRef: "amit-remote " + ref,
	}
	a.runs[key] = r
	a.order = append(a.order, key)
	return r
}

// observeGenerationLocked applies §4's epoch rule: only receipts pin. The
// pinned generation is the newest receipt-proven observation; a later
// receipt with a different generation means the session regenerated and the
// published epoch follows it. An invalid generation string is never pinned.
func (a *Attachment) observeGenerationLocked(gen string) {
	if gen != "" && protocol.ValidEpoch(gen) && gen != a.epoch {
		a.epoch = gen
	}
}

// consumeLocked refreshes every retained run from the durable seam. Called
// under a.mu by Inspect, Lookup, CancelExact, AcknowledgeResult, and the
// Submit poll — the seam is pull-based; there is no background reader and
// no path that touches the run map outside a.mu.
func (a *Attachment) consumeLocked() {
	for _, k := range a.order {
		if r, ok := a.runs[k]; ok {
			a.refreshRunLocked(r)
		}
	}
}

// refreshRunLocked re-reads the durable evidence for one run: the receipt
// (admission) and the event stream (terminals). Errors from the seam are
// never guessed around: a run whose receipt cannot be read stays unconfirmed
// (uncertain), a run whose events cannot be read keeps its current state.
func (a *Attachment) refreshRunLocked(r *run) {
	if r.acked {
		return
	}
	if !r.confirmed {
		rc, err := a.dir.readReceipt(r.ref)
		switch {
		case err == nil && rc != nil:
			r.confirmed = true
			r.notFound = nil
			a.observeGenerationLocked(rc.SessionGeneration)
		case err == nil:
			// Absent: no new information.
		default:
			// Unreadable or foreign-protocol: never consume it as evidence.
			r.notFound = err
		}
	}
	if !r.terminal {
		a.applyEventsLocked(r)
	}
}

// applyEventsLocked applies the ref's event stream (§3). The FIRST terminal
// event is final for the ref; later lines never overwrite it. A refused
// event maps to the endpoint's typed refusal codes: fire-time window expiry
// with a receipt present → expired (A3: the receipt STAYS — admission
// happened, execution was refused), generation mismatch → stale_epoch,
// busy → busy, anything else → native_error.
func (a *Attachment) applyEventsLocked(r *run) {
	events, err := a.dir.readEvents(r.ref)
	if err != nil {
		// A2: a rotated or temporarily unreadable log is tolerated — the
		// run keeps its current state and the next refresh retries.
		return
	}
	for _, ev := range events {
		if ev.Ref != "" && ev.Ref != r.ref {
			continue
		}
		switch ev.Event {
		case "started":
			if !r.terminal {
				r.state = protocol.StateRunning
			}
		case "completed":
			if r.terminal {
				break
			}
			r.terminal = true
			r.state = protocol.StateCompleted
			r.text.Reset()
			r.text.WriteString(ev.Text)
			a.emitLocked(core.NativeEvent{
				Type:   core.EventRunCompleted,
				Key:    r.key,
				RunID:  r.runID,
				Result: r.result(),
			})
		case "failed":
			if r.terminal {
				break
			}
			r.terminal = true
			r.state = protocol.StateFailed
			r.errText = ev.Error
			a.emitLocked(core.NativeEvent{
				Type:   core.EventRunFailed,
				Key:    r.key,
				RunID:  r.runID,
				Result: r.result(),
			})
		case "cancelled":
			if r.terminal {
				break
			}
			r.terminal = true
			r.state = protocol.StateCancelled
			a.emitLocked(core.NativeEvent{
				Type:  core.EventRunCancelled,
				Key:   r.key,
				RunID: r.runID,
			})
		case "refused":
			if r.terminal {
				break
			}
			r.terminal = true
			r.state = protocol.StateRejected
			r.refused = refusalCodeFor(ev.Reason)
			if ev.Reason == "generation" {
				// §4: the pinned generation is proven stale. The refused
				// event carries no live generation (the extension refused
				// pre-delivery and wrote no receipt), so the adapter drops
				// back to the `unpinned` sentinel; the next delivered
				// request's receipt re-pins the live generation.
				a.epoch = ""
			}
		}
		if r.terminal {
			break // first-terminal-wins (§3): later lines are not evidence
		}
	}
}

// refusalCodeFor maps a §3 refused reason to the endpoint's typed codes.
func refusalCodeFor(reason string) protocol.Code {
	switch reason {
	case "expired":
		return protocol.CodeExpired
	case "generation":
		return protocol.CodeStaleEpoch
	case "busy":
		return protocol.CodeBusy
	default:
		return protocol.CodeNativeError
	}
}

// prune bounds the retained run map: terminal+acked runs are dropped FIFO.
func (a *Attachment) pruneLocked() {
	if len(a.runs) <= maxRetained {
		return
	}
	for len(a.runs) > maxRetained && len(a.order) > 0 {
		oldest := a.order[0]
		a.order = a.order[1:]
		if r, ok := a.runs[oldest]; ok && r.state.Terminal() && r.acked {
			delete(a.runs, oldest)
		}
	}
}

// emitLocked fans one event out to subscribers. Callers hold a.mu.
func (a *Attachment) emitLocked(ev core.NativeEvent) {
	for _, fn := range a.listeners {
		go fn(ev)
	}
}

// result is the ONLY place a protocol.Result is constructed in this
// package, so the bounding point (MaxResultBytes, UTF-8-rune safe) is one.
func (r *run) result() *protocol.Result {
	res := &protocol.Result{Text: r.text.String(), Error: r.errText}
	if r.nativeRef != "" {
		res.NativeRef = r.nativeRef
	}
	if len(res.Text) > protocol.MaxResultBytes {
		n := protocol.MaxResultBytes
		for n > 0 && !utf8.RuneStart(res.Text[n]) {
			n--
		}
		res.Text = res.Text[:n]
		res.Truncated = true
	}
	return res
}

// Inspect implements core.Attachment. The published epoch is the pinned
// generation, or the `unpinned` sentinel before the first receipt (§4/A4 —
// an empty epoch would defeat stale-epoch protection because "" == "").
func (a *Attachment) Inspect() protocol.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consumeLocked()
	att, status := "live", "idle"
	if live := a.dir.liveness(a.now()); !live.live {
		att, status = "offline", "offline"
	}
	epoch := a.epoch
	if epoch == "" {
		epoch = SentinelUnpinned
	}
	// No interaction surface is wired (Respond is already_resolved), so
	// PendingInteraction is always nil.
	return protocol.Session{
		Schema:             protocol.SchemaSession,
		TargetID:           a.target,
		Epoch:              epoch,
		Harness:            "amit",
		DisplayName:        "amit " + a.handle,
		Attachment:         att,
		Status:             status,
		PendingInteraction: nil,
		// §7 capability projection: Steer is FALSE (v1 is followUp-only; the
		// endpoint's D1 gate refuses deliver=steer pre-adapter and the
		// adapter refuses it again pre-side-effect), cancel/approve/question
		// are false (no native seam; keystrokes are forbidden), terminal is
		// unavailable.
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: true, CancelRequest: false, Steer: false,
			ApproveTool: false, AnswerQuestion: false, Terminal: "unavailable",
		},
		// §7: the adapter never claims `admitted` in Inspect — the per-ref
		// receipt is the admission proof, not a session-wide capability.
		Evidence:   &protocol.Evidence{Submit: protocol.EvidenceSubmitted, Completion: "run_terminal"},
		ObservedAt: protocol.FormatTime(a.now()),
	}
}

// Submit implements core.Attachment.
//
// Contract flow (§1, §2): publish the request atomically and create-new
// (a duplicate ref is a positive pre-send refusal — AMQ must never
// double-fire), then poll briefly for the receipt. Admission is
// RECEIPT-GATED: Admitted:true is returned only when receipts/<ref>.json
// exists (9b). With a live bridge and no receipt yet the submit is
// UNCERTAIN (the extension may still deliver; AMQ never replays a ref);
// with no live bridge the submit FAILED pre-side-effect — for a fresh
// submit the liveness gate runs BEFORE the request file is written, so a
// dead bridge never leaves a file behind.
func (a *Attachment) Submit(req core.BoundRequest) (core.Admission, error) {
	a.mu.Lock()
	// §4/A4 epoch gate: while unpinned (a.epoch == "") the caller's epoch is
	// the sentinel (it read Inspect), and the submit goes out with an EMPTY
	// epoch_hint (first contact, no check). Once a receipt pinned a
	// generation, the caller's epoch must match it exactly.
	if a.epoch != "" && req.Epoch != a.epoch {
		// The endpoint's admissible check compares cmd.Epoch against
		// Inspect().Epoch, so this only fires on a race; it mirrors the
		// stale-epoch refusal positively either way.
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeStaleEpoch, Message: "epoch does not match the pinned session generation; re-inspect"}, nil
	}
	if r, ok := a.runs[req.Key]; ok {
		// Retry of an already-published submit (endpoint retries carry the
		// same ref). Re-read the seam: the receipt may have landed since.
		// NEVER return Admitted without a receipt (9b).
		a.refreshRunLocked(r)
		rid := r.runID
		if r.confirmed {
			a.mu.Unlock()
			return core.Admission{Admitted: true, RunID: rid}, nil
		}
		live := a.dir.liveness(a.now())
		a.mu.Unlock()
		if !live.live {
			// §2: no receipt + stale/absent liveness → FAILED. The request
			// file stays for a later bridge (the adapter owns it and never
			// deletes); the refusal is positive and terminal.
			return core.Admission{}, protocol.Refuse(protocol.CodeAttachmentLost,
				"no live amit-remote bridge for handle %q (bridge.liveness %s); request %s stays for a later bridge", a.handle, live.reason, r.ref)
		}
		return core.Admission{RunID: rid}, fmt.Errorf("amit: receipt for %s not yet observed; submission uncertain", r.ref)
	}
	if req.Input.Deliver == protocol.DeliverSteer {
		// §7: v1 is followUp-only. The endpoint's D1 gate already refuses
		// deliver=steer; this is the belt-and-braces adapter-side refusal,
		// before any file write.
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeUnsupported, Message: "deliver=steer is disabled in v1 (amit advertises no Steer); use deliver=turn"}, nil
	}
	// Fresh submit: the liveness gate is PRE-SIDE-EFFECT — nobody listening
	// means nothing is written and the refusal is positive (§2).
	if live := a.dir.liveness(a.now()); !live.live {
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeAttachmentLost, Message: fmt.Sprintf("no live amit-remote bridge for handle %q (bridge.liveness %s)", a.handle, live.reason)}, nil
	}
	epochHint := a.epoch // §4: pinned generation as hint; "" (unpinned) = first contact, no check
	a.mu.Unlock()

	// File I/O outside a.mu: the mutex guards correlation state, not the
	// seam. A concurrent same-key submit cannot happen (the endpoint's
	// per-runtime reservation serializes dispatches), and a bind after the
	// write below re-checks the map under the lock.
	ref := clientRef(req.Key)
	preq := deliverRequest{
		Ref:       ref,
		Text:      req.Input.Text,
		DeliverAs: "followUp", // §1/§6: v1 delivers followUp only
		NotAfter:  req.NotAfter,
		EpochHint: epochHint,
		CreatedAt: protocol.FormatTime(a.now()),
	}
	err := a.dir.publishRequest(preq)

	a.mu.Lock()
	r, ok := a.runs[req.Key]
	if !ok {
		r = a.bindRunLocked(req.Key, req.Epoch)
	}
	if err != nil {
		if errors.Is(err, ErrAlreadyDelivered) {
			// The request file already exists (a previous process published
			// it and crashed before binding, or a duplicate raced the
			// reservation). The ref was already delivered: bind, never
			// rewrite (O_EXCL, §1), classify from the durable seam.
			a.refreshRunLocked(r)
			rid := r.runID
			if r.confirmed {
				a.mu.Unlock()
				return core.Admission{Admitted: true, RunID: rid}, nil
			}
			live := a.dir.liveness(a.now())
			a.mu.Unlock()
			if !live.live {
				return core.Admission{}, protocol.Refuse(protocol.CodeAttachmentLost,
					"no live amit-remote bridge for handle %q (bridge.liveness %s); request %s stays for a later bridge", a.handle, live.reason, r.ref)
			}
			return core.Admission{RunID: rid}, fmt.Errorf("amit: receipt for %s not yet observed; submission uncertain", r.ref)
		}
		a.mu.Unlock()
		// Pre-send/ambiguous seam failure: nothing provably reached the
		// extension. Return the error so the endpoint records uncertain —
		// the send primitive cannot report WHY, so a refusal here would
		// guess (the text may still be delivered by a later bridge scan).
		return core.Admission{}, err
	}

	// Receipt poll: the extension writes the receipt when it delivers (§2).
	// The window is REAL wall time (how long this call may block the
	// endpoint), deliberately not the frozen test clock — a.now stays for
	// timestamps and liveness freshness.
	deadline := time.Now().Add(a.submitWait)
	for {
		a.refreshRunLocked(r)
		if r.confirmed {
			rid := r.runID
			a.mu.Unlock()
			return core.Admission{Admitted: true, RunID: rid}, nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		a.mu.Unlock()
		time.Sleep(submitPollStep)
		a.mu.Lock()
	}
	rid := r.runID
	live := a.dir.liveness(a.now())
	a.mu.Unlock()
	if !live.live {
		// The bridge died mid-window (the file is already published; §2's
		// in-flight row: the file stays for a later bridge).
		return core.Admission{}, protocol.Refuse(protocol.CodeAttachmentLost,
			"no live amit-remote bridge for handle %q (bridge.liveness %s); request %s stays for a later bridge", a.handle, live.reason, r.ref)
	}
	return core.Admission{RunID: rid}, fmt.Errorf("amit: receipt for %s not yet observed; submission uncertain", r.ref)
}

// Lookup implements core.Attachment. Recovery already bound every
// receipt-backed key at attach time; a key with nothing retained is
// EvidenceUnknown — pi's send primitive cannot prove non-admission, so a
// lost submit and a never-submitted key look identical (never EvidenceNone
// for an unretained key).
func (a *Attachment) Lookup(key requests.Key, epoch string) (core.Evidence, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consumeLocked()
	r, ok := a.runs[key]
	if !ok {
		a.lateBindLocked(key)
		r, ok = a.runs[key]
	}
	if !ok {
		return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
	}
	if r.epoch != "" && r.epoch != epoch {
		// Bound under a different epoch: no evidence for THIS epoch's
		// correlation (the endpoint keeps its record uncertain).
		return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
	}
	if r.notFound != nil {
		// The seam is unreadable (not merely absent): report the error so
		// the endpoint keeps the record uncertain instead of guessing.
		return core.Evidence{}, r.notFound
	}
	ev := core.Evidence{Known: true, RunID: r.runID, State: r.state}
	if r.acked {
		// Result released: nothing retained, admission proven.
		ev.Class = core.EvidenceNone
		ev.Admitted = true
		return ev, nil
	}
	if r.refused != "" {
		// §6/A3: definitive native refusal. With a receipt present the
		// admission is PROVEN (the receipt stays across a fire-time
		// refusal); RefusalCode routes the endpoint to the typed refusal
		// (rejected+code), never to a silent dispatch and never to
		// evidence of non-admission.
		ev.Class = core.EvidenceHistoryTerminated
		ev.Admitted = r.confirmed
		ev.State = protocol.StateRejected
		ev.RefusalCode = r.refused
		return ev, nil
	}
	if r.terminal {
		// A terminal turn event proves the turn ran — admission happened
		// regardless of whether the receipt file survives (A2 tolerance).
		ev.Class = core.EvidenceHistoryTerminated
		ev.Admitted = true
		ev.Result = r.result()
		return ev, nil
	}
	if r.confirmed {
		// §5: receipt present, no terminal event (rotated/absent log
		// included) → accepted, confirmed-running. NEVER uncertain: the
		// receipt is positive admission evidence.
		ev.Class = core.EvidenceConfirmed
		ev.Admitted = true
		return ev, nil
	}
	// No receipt: uncertain (§5). The record keeps correlating.
	ev.Class = core.EvidenceUnknown
	return ev, nil
}

// CancelExact implements core.Attachment. There is no native cancel seam
// (keystrokes are forbidden, §7): a cancel is intent only and resolved when
// the run is already terminal.
func (a *Attachment) CancelExact(key requests.Key, epoch string) (core.CancelEvidence, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consumeLocked()
	r, ok := a.runs[key]
	if !ok {
		a.lateBindLocked(key)
		r, ok = a.runs[key]
	}
	if !ok {
		return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "amit adapter has no native cancel seam"}, nil
	}
	if r.epoch != "" && r.epoch != epoch {
		return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "epoch mismatch"}, nil
	}
	if r.state.Terminal() {
		return core.CancelEvidence{Disposition: protocol.CancelNoopTerminal}, nil
	}
	return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "amit adapter has no native cancel seam; the run resolves from the amit-remote event stream"}, nil
}

// Respond implements core.Attachment: no interaction surface is wired, so
// every answer is already-resolved.
func (a *Attachment) Respond(key requests.Key, epoch, interactionID, option string) (protocol.Code, error) {
	return protocol.CodeAlreadyResolved, nil
}

// AcknowledgeResult implements core.Attachment: releases the retained
// terminal evidence for the key once the digest matches exactly.
func (a *Attachment) AcknowledgeResult(key requests.Key, epoch, digest string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consumeLocked()
	r, ok := a.runs[key]
	if !ok {
		a.lateBindLocked(key)
		r, ok = a.runs[key]
	}
	if !ok || (r.epoch != "" && r.epoch != epoch) || !r.state.Terminal() || r.acked {
		return
	}
	if digest == "" || digest != protocol.EvidenceDigestStable(r.result()) {
		return
	}
	r.acked = true
	r.text.Reset()
	r.errText = ""
	a.pruneLocked()
}

// Subscribe implements core.Attachment.
func (a *Attachment) Subscribe(fn func(core.NativeEvent)) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	id := a.nextID
	a.listeners[id] = fn
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.listeners, id)
	}
}
