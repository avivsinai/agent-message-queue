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
	// eventsRefused is a §9 refusal of the event stream (foreign protocol).
	// Unlike an A2 unreadable log it is surfaced by Lookup: a refused stream
	// is never "no events" (never confirmed-running).
	eventsRefused error
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
	a.recover()
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

// recover rebuilds runs from receipts/ + events/ (§5). Receipts are
// iterated oldest-first so the pinned epoch ends at the newest receipt's
// generation (the freshest receipt-proven observation). 9a: the seam reads
// happen without the lock; the bind+apply section takes it. Junk files
// (unparseable, foreign protocol, undecodable ref) are skipped, never
// guessed into evidence.
func (a *Attachment) recover() {
	// 9a: all seam reads (listReceipts + per-ref event logs) happen BEFORE
	// the lock; recovery then binds and applies under a.mu.
	type seeded struct {
		key    requests.Key
		rc     receipt
		events []event
		evErr  error
	}
	var seeds []seeded
	for _, rc := range a.dir.listReceipts() {
		creatorHost, targetID, requestID, derr := protocol.DecodeRef(rc.Ref)
		if derr != nil {
			continue
		}
		key := requests.Key{CreatorHost: creatorHost, TargetID: targetID, RequestID: requestID}
		events, evErr := a.dir.readEvents(rc.Ref)
		seeds = append(seeds, seeded{key: key, rc: rc, events: events, evErr: evErr})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, sd := range seeds {
		if _, ok := a.runs[sd.key]; ok {
			continue
		}
		rc := sd.rc
		r := a.bindRunLocked(sd.key, "") // recovered history: epoch wildcard
		r.confirmed = true
		a.observeGenerationLocked(rc.SessionGeneration)
		if sd.evErr != nil {
			// §9: a refused event stream (foreign protocol) is never "no
			// events" — bind with the refusal so Lookup surfaces it.
			r.eventsRefused = sd.evErr
		} else {
			a.applyEventsLocked(r, sd.events)
		}
	}
}

// lateBind runs the recovery scan for one unseen key WITHOUT the lock (9a:
// the seam reads happen outside a.mu), then binds under it. A receipt for a
// ref whose run is not yet in the map (published by a prior process, or
// seeded/published between calls) binds a wildcard-epoch run with the
// receipt-proven state — history never hides behind an in-memory miss (§5).
func (a *Attachment) lateBind(key requests.Key) {
	a.mu.Lock()
	_, ok := a.runs[key]
	a.mu.Unlock()
	if ok {
		return
	}
	// FS reads, no lock.
	rc, rcErr := a.dir.readReceipt(clientRef(key))
	var events []event
	var evErr error
	if rc != nil {
		events, evErr = a.dir.readEvents(rc.Ref)
	}
	// Bind+apply under the lock.
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lateBindLocked(key, rc, rcErr, events, evErr)
}

// lateBindLocked binds one unseen key from an already-read observation.
// Called under a.mu.
func (a *Attachment) lateBindLocked(key requests.Key, rc *receipt, rcErr error, events []event, evErr error) {
	if _, ok := a.runs[key]; ok {
		return
	}
	switch {
	case rc != nil:
		r := a.bindRunLocked(key, "") // recovered history: epoch wildcard
		r.confirmed = true
		a.observeGenerationLocked(rc.SessionGeneration)
		if evErr != nil {
			// §9: a refused event stream (foreign protocol) is never "no
			// events" — bind with the refusal so Lookup surfaces it.
			r.eventsRefused = evErr
		} else {
			a.applyEventsLocked(r, events)
		}
	case rcErr != nil:
		// Unreadable or foreign-protocol receipt (§9): bind unconfirmed with
		// the seam error so Lookup surfaces it instead of guessing it into
		// evidence — or silently ignoring a record the endpoint still holds.
		r := a.bindRunLocked(key, "")
		r.notFound = rcErr
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

// observeGenerationLocked applies §4's epoch rule: only receipts pin. Any
// valid, different generation replaces the current pin — the code does NOT
// order observations by receipt time (P2-A, review 816-r3): with two
// concurrent readers, a stale read can momentarily regress the pin. The
// regression is self-healing: the extension answers a stale hint with
// refused(generation), which unpins to the sentinel and lets the next
// receipt re-pin. An invalid generation string is never pinned.
func (a *Attachment) observeGenerationLocked(gen string) {
	if gen != "" && protocol.ValidEpoch(gen) && gen != a.epoch {
		a.epoch = gen
	}
}

// seamObservation is one run's freshly read durable evidence (9a: the FILE
// I/O happens outside a.mu; only this immutable snapshot crosses the lock).
type seamObservation struct {
	ref      string
	receipt  *receipt // nil = absent or already confirmed
	rcErr    error    // unreadable/foreign-protocol receipt (never guessed around)
	events   []event
	evErr    error // unreadable/foreign-protocol event stream (A2 vs §9)
	readEvts bool  // events were read (skip when terminal already known)
}

// consume refreshes every retained run from the durable seam WITHOUT the
// lock (9a: no filesystem call under a.mu). It snapshots which refs need
// reading, does the reads, then takes the lock once to apply. The seam is
// pull-based; there is no background reader.
func (a *Attachment) consume() {
	a.mu.Lock()
	type pending struct {
		r      *run
		readEv bool
		readRc bool
	}
	var queue []pending
	for _, k := range a.order {
		r, ok := a.runs[k]
		if !ok || r.acked {
			continue
		}
		readRc := !r.confirmed
		readEv := !r.terminal
		if readRc || readEv {
			queue = append(queue, pending{r: r, readRc: readRc, readEv: readEv})
		}
	}
	a.mu.Unlock()

	if len(queue) == 0 {
		return
	}
	obs := make(map[string]*seamObservation, len(queue))
	for _, p := range queue {
		o := &seamObservation{ref: p.r.ref}
		if p.readRc {
			rc, err := a.dir.readReceipt(p.r.ref)
			o.receipt, o.rcErr = rc, err
		}
		if p.readEv {
			evs, err := a.dir.readEvents(p.r.ref)
			o.events, o.evErr, o.readEvts = evs, err, true
		}
		obs[p.r.ref] = o
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range queue {
		r, ok := a.runs[p.r.key]
		if !ok || r.acked {
			continue // pruned or acked while we read
		}
		a.applyObservationLocked(r, obs[p.r.ref])
	}
}

// applyObservationLocked applies a seam observation to a run under a.mu.
func (a *Attachment) applyObservationLocked(r *run, o *seamObservation) {
	if r.acked {
		return
	}
	if o == nil || o.ref != r.ref {
		return
	}
	if o.receipt != nil || o.rcErr != nil {
		switch {
		case o.receipt != nil:
			r.confirmed = true
			r.notFound = nil
			r.eventsRefused = nil
			a.observeGenerationLocked(o.receipt.SessionGeneration)
		case o.rcErr != nil && !r.confirmed:
			// Unreadable or foreign-protocol: never consume it as evidence.
			// Only an unconfirmed run records the error (review 816-r3
			// P2-B): a stale failed read landing after a successful receipt
			// must not resurrect notFound — consume() never re-reads a
			// confirmed run's receipt, so that would wedge the run into
			// permanent uncertainty.
			r.notFound = o.rcErr
		}
	}
	if o.readEvts {
		switch {
		case o.evErr != nil:
			// §9/A2 split: a rotated or temporarily unreadable log is
			// tolerated (A2 — keep state, retry next refresh), but a
			// foreign-protocol stream is REFUSED, never read as "no events"
			// (recovery row 3 would map that to confirmed-running, hiding a
			// terminal state).
			r.eventsRefused = o.evErr
		case len(o.events) > 0:
			// A present, protocol-validated stream clears a previous
			// transient refusal (review 816-r3 P2-C). Only this case
			// clears: an ABSENT log (rotation, truncation — readEvents
			// returns no error and no events) must never unmask a proven
			// foreign seam as "no events" → confirmed-running
			// (review 816-r4 P1), and a proven-foreign refusal therefore
			// needs a present v1 stream to lift.
			r.eventsRefused = nil
			a.applyEventsLocked(r, o.events)
		default:
			// Absent-or-empty log: keep state and any prior refusal
			// exactly as they are (A2), never clear, never re-apply.
			a.applyEventsLocked(r, o.events)
		}
	}
}

// applyEventsLocked applies the ref's event stream (§3). The FIRST terminal
// event is final for the ref; later lines never overwrite it. A refused
// event maps to the endpoint's typed refusal codes: fire-time window expiry
// with a receipt present → expired (A3: the receipt STAYS — admission
// happened, execution was refused), generation mismatch → stale_epoch,
// busy → busy, anything else → native_error.
func (a *Attachment) applyEventsLocked(r *run, events []event) {
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
	a.consume() // 9a: seam reads outside a.mu; apply under it
	a.mu.Lock()
	epoch := a.epoch
	if epoch == "" {
		epoch = SentinelUnpinned
	}
	a.mu.Unlock()
	att, status := "live", "idle"
	if live := a.dir.liveness(a.now()); !live.live { // 9a: FS read, no lock
		att, status = "offline", "offline"
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
	// 9a: the existing-run seam re-read happens WITHOUT the lock first.
	a.consume()

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
	epochHint := a.epoch // §4: pinned generation as hint; "" (unpinned) = first contact, no check
	// The hint is captured in the SAME critical section as the §4 gate
	// above (review 816-r3 P1): re-reading it after the lock-free liveness
	// check let a concurrent re-pin/refuse publish a generation the gate
	// never validated — or omit the hint entirely on a refused(generation),
	// which the extension defines as "no check" on a just-proven-stale
	// request.
	if r, ok := a.runs[req.Key]; ok {
		// Retry of an already-published submit (endpoint retries carry the
		// same ref); consume() above already re-read the seam so the
		// receipt may have landed since. NEVER return Admitted without a
		// receipt (9b).
		rid := r.runID
		if r.confirmed {
			a.mu.Unlock()
			return core.Admission{Admitted: true, RunID: rid}, nil
		}
		a.mu.Unlock()
		// 9a: liveness is a filesystem read — take it without the lock.
		live := a.dir.liveness(a.now())
		if !live.live {
			// §2: no receipt + stale/absent liveness. The request file
			// stays for a later bridge (the adapter owns it and never
			// deletes); delivery is still possible, so this is an
			// UNCERTAIN-shaped refusal — the endpoint keeps the correlation
			// (admissionCause maps any native error to attachment_lost).
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
	// means nothing is written and the refusal is positive (§2). 9a: the
	// filesystem read happens without the lock.
	a.mu.Unlock()
	if live := a.dir.liveness(a.now()); !live.live {
		return core.Admission{Code: protocol.CodeAttachmentLost, Message: fmt.Sprintf("no live amit-remote bridge for handle %q (bridge.liveness %s)", a.handle, live.reason)}, nil
	}
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
	rid := r.runID
	refOfRun := r.ref
	a.mu.Unlock()
	if err != nil {
		if errors.Is(err, ErrAlreadyDelivered) {
			// The request file already exists (a previous process published
			// it and crashed before binding, or a duplicate raced the
			// reservation). The ref was already delivered: bind, never
			// rewrite (O_EXCL, §1), classify from the durable seam.
			o := a.readSeamFor(refOfRun) // 9a: FS reads without the lock
			a.mu.Lock()
			a.applyObservationLocked(r, o)
			rid := r.runID
			if r.confirmed {
				a.mu.Unlock()
				return core.Admission{Admitted: true, RunID: rid}, nil
			}
			a.mu.Unlock()
			live := a.dir.liveness(a.now())
			if !live.live {
				// UNCERTAIN-shaped: the file is already published, a later
				// bridge can still deliver; admissionCause keeps the
				// correlation (never a terminal rejected record).
				return core.Admission{}, protocol.Refuse(protocol.CodeAttachmentLost,
					"no live amit-remote bridge for handle %q (bridge.liveness %s); request %s stays for a later bridge", a.handle, live.reason, refOfRun)
			}
			return core.Admission{RunID: rid}, fmt.Errorf("amit: receipt for %s not yet observed; submission uncertain", refOfRun)
		}
		// Pre-send/ambiguous seam failure: nothing provably reached the
		// extension. Return the error so the endpoint records uncertain —
		// the send primitive cannot report WHY, so a refusal here would
		// guess (the text may still be delivered by a later bridge scan).
		return core.Admission{}, err
	}

	// Receipt poll: the extension writes the receipt when it delivers (§2).
	// The window is REAL wall time (how long this call may block the
	// endpoint), deliberately not the frozen test clock — a.now stays for
	// timestamps and liveness freshness. 9a: each poll step reads the seam
	// WITHOUT the lock, then re-locks to apply.
	deadline := time.Now().Add(a.submitWait)
	confirmed := false
	for {
		o := a.readSeamFor(refOfRun)
		a.mu.Lock()
		a.applyObservationLocked(r, o)
		confirmed = r.confirmed
		a.mu.Unlock()
		if confirmed {
			return core.Admission{Admitted: true, RunID: rid}, nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(submitPollStep)
	}
	live := a.dir.liveness(a.now())
	if !live.live {
		// The bridge died mid-window (the file is already published; §2's
		// in-flight row: the file stays for a later bridge). Delivery is
		// still possible from a later bridge scan, so the refusal is
		// UNCERTAIN-shaped (admissionCause keeps the correlation).
		return core.Admission{}, protocol.Refuse(protocol.CodeAttachmentLost,
			"no live amit-remote bridge for handle %q (bridge.liveness %s); request %s stays for a later bridge", a.handle, live.reason, refOfRun)
	}
	return core.Admission{RunID: rid}, fmt.Errorf("amit: receipt for %s not yet observed; submission uncertain", refOfRun)
}

// readSeamFor reads one run's durable evidence by ref WITHOUT the lock
// (9a): a single-run observation for the Submit poll and retry paths.
func (a *Attachment) readSeamFor(ref string) *seamObservation {
	o := &seamObservation{ref: ref}
	o.receipt, o.rcErr = a.dir.readReceipt(ref)
	evs, evErr := a.dir.readEvents(ref)
	o.events, o.evErr, o.readEvts = evs, evErr, true
	return o
}

// Lookup implements core.Attachment. Recovery already bound every
// receipt-backed key at attach time; a key with nothing retained is
// EvidenceUnknown — pi's send primitive cannot prove non-admission, so a
// lost submit and a never-submitted key look identical (never EvidenceNone
// for an unretained key).
func (a *Attachment) Lookup(key requests.Key, epoch string) (core.Evidence, error) {
	a.consume()     // 9a: seam reads outside a.mu; apply under it
	a.lateBind(key) // 9a: same, for a key not yet bound
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.runs[key]
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
	if r.eventsRefused != nil {
		// §9: the event stream was refused (foreign protocol). Surface the
		// refusal — it must never silently read as "no events".
		return core.Evidence{}, r.eventsRefused
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
	a.consume()     // 9a: seam reads outside a.mu; apply under it
	a.lateBind(key) // 9a: same, for a key not yet bound
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.runs[key]
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
	a.consume()     // 9a: seam reads outside a.mu; apply under it
	a.lateBind(key) // 9a: same, for a key not yet bound
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.runs[key]
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
