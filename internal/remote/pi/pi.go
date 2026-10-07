// Package pi is the pi coding-agent adapter factory. It registers the
// kind:"pi" factory under the named-factory registry so a manifest entry
// `{"kind":"pi","target":"pi-1","config":{"handle":"<session handle>"}}`
// builds an Attachment without editing serve.
//
// The adapter implements the pi-bridge protocol v1. The bridge is a pi
// extension; this adapter is extension-only: it does not spawn, own, or
// supervise the pi process (ADR invariant 1 — up supervises serve only;
// serve never owns a harness). All coordination happens through the
// protocol's file seam under <AM_ROOT>/agents/<handle>/extensions/pi-bridge/:
//
//	requests/<ref>.json   written by THIS adapter (atomic, create-new)
//	receipts/<ref>.json   read by THIS adapter (admission proof)
//	events/<ref>.jsonl    read by THIS adapter (terminal evidence, interactions)
//	answers/<hash>.json   written by THIS adapter (approval answers, revision 4)
//	bridge.liveness       read by THIS adapter (heartbeat freshness)
//
// Ownership (protocol: ownership): the adapter writes ONLY requests/ and
// answers/, and reads ONLY receipts/, events/, and bridge.liveness. It never
// touches pi internals, the doorbell layer, or any other extension directory.
//
// Evidence: pi's sendUserMessage returns void and swallows rejections, and
// v1 has no native admission primitive — the per-ref RECEIPT is the only
// admission proof (protocol: receipts). Until a receipt is observed the adapter
// publishes the address epoch `unpinned.<generation>` for the live generation,
// or the sentinel `unpinned` without one (protocol: session generation and
// epoch); the first receipt pins the session generation. A key the adapter
// retains nothing about is EvidenceUnknown, never EvidenceNone.
package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// config is the adapter-specific config block for a pi manifest entry.
type config struct {
	// Handle is the AMQ session handle whose bridge extension directory
	// this adapter coordinates with. The (root, handle) pair must match the
	// session identity entry the pi-side extension resolved
	// (protocol: identity and layout; the identity entry is the source of
	// truth, no env fallback). The manifest is AMQ-owned (protocol: ownership), so the handle is
	// stated here explicitly — never derived from AM_ME/AM_ROOT env.
	Handle string `json:"handle"`
	// UpgradeHint is the remedy the owner is told when the live bridge is
	// too old and does not publish its own. The party that declares the
	// target sets it for a pi build that ships its own extension.
	UpgradeHint string `json:"upgrade_hint,omitempty"`
}

// run is one bound request tracked by the adapter. The durable record is
// the extension's (receipts + events); this struct is the in-memory
// correlation the adapter rebuilds from those files on attach (protocol: adapter recovery and evidence).
type run struct {
	key     requests.Key
	ref     string
	runID   string
	epoch   string // epoch the submit was bound under; "" = recovered history (any epoch matches)
	state   protocol.State
	refused protocol.Code // set by a definitive refused event, mapped to a typed code (protocol: events; adapter recovery and evidence)
	// refusedMsg is the refused event's reason and error, for the owner.
	refusedMsg string

	text      strings.Builder
	errText   string
	nativeRef string

	confirmed bool  // receipt observed — admission proven (protocol: receipts)
	terminal  bool  // first terminal event consumed (first-terminal-wins; protocol: events)
	uncertain bool  // the terminal event is `uncertain`: the extension has no native outcome
	acked     bool  // endpoint acknowledged the retained result
	notFound  error // last seam error that blocks evidence (never guessed around)
	// eventsRefused is a protocol-string refusal of the event stream (foreign protocol).
	// Unlike an unreadable log it is surfaced by Lookup: a refused stream
	// is never "no events" (never confirmed-running).
	eventsRefused error

	// gen is the session generation of the ref's receipt: the only
	// generation in which the extension can apply an answer for it.
	gen string

	// Tool approvals (protocol: bridge revision 4), rebuilt from the event
	// stream like every other state. interactions holds every valid
	// approval the stream raised, open or closed, so a re-read never raises
	// one twice and a replayed answer is recognized after it closes. open
	// is the open ones, oldest first; open[0] is the one the endpoint
	// shows. outcomes is how each closed one ended, by id, with no cap:
	// ResolvedInteraction serves the endpoint a resolution it never
	// received, however many approvals closed after it.
	interactions map[string]*openInteraction
	open         []*openInteraction
	outcomes     map[string]protocol.Resolution
}

// openInteraction is one approval raised by an interaction line. The
// manifest hash and expiry bind an answer to exactly that tool call; they
// travel into the answer file and never leave the adapter otherwise.
// approve is the approve option the line lets a remote face give, or "".
type openInteraction struct {
	id           string
	prompt       string
	approve      string
	reject       string
	manifestHash string
	expiresAt    time.Time
}

// Attachment implements core.Attachment over the pi-bridge file seam. It
// never touches the local editor and owns no process.
type Attachment struct {
	mu     sync.Mutex
	target string
	handle string
	// upgradeHint is the manifest's remedy for a too-old bridge.
	upgradeHint string
	dir         bridgeDir
	// epoch is the receipt-pinned session generation (protocol: session generation and epoch). Empty means "no
	// receipt observed yet" — Inspect publishes the address epoch
	// `unpinned.<generation>` of the live generation, and a first-contact
	// submit addresses exactly the generation its caller's epoch names.
	epoch string
	// epochPID is the pi process that wrote the pinning receipt. A
	// generation lives inside one process, so a live bridge with another
	// pid proves the pin stale (see dropStalePinLocked). 0 = unknown.
	epochPID int

	runs      map[requests.Key]*run
	order     []requests.Key // bind order, for bounded pruning + deterministic consume
	listeners map[int]func(core.NativeEvent)
	nextID    int
	// emitq is the FIFO of native events not yet delivered. One drainer
	// goroutine delivers them in order outside a.mu, so a question and its
	// resolution read in one pass reach the endpoint in that order.
	emitq    []core.NativeEvent
	emitting bool
	// live is the latest liveness read, taken outside a.mu by consume. It
	// decides whether a remote answer can apply to an open approval.
	live livenessState
	now  func() time.Time
	// submitWait bounds the post-publication receipt poll in Submit. The
	// extension writes the receipt when it delivers (protocol: receipts); a poll window
	// that expires with a live bridge leaves the record UNCERTAIN, never
	// rejected (AMQ never replays a ref; protocol: receipts).
	submitWait time.Duration
}

// Version is stamped by the binary. It names the release tag an owner
// installs the extension from when the live bridge is too old.
var Version = "dev"

// oldBridgeMessage is the refusal an owner sees when the live extension
// predates MinBridgeRevision.
func oldBridgeMessage(handle string, live livenessState, hint string) string {
	have := "publishes no bridge_revision"
	if live.revision > 0 {
		have = fmt.Sprintf("is bridge_revision %d", live.revision)
	}
	// The remedy the bridge publishes names its own install path; then the
	// manifest's hint; then this repo's stock extension. A supplied remedy
	// is labeled with its source, because a stale build keeps publishing it.
	remedy := ""
	if r := remedyText(live.upgrade); r != "" {
		remedy = r + " (suggested by the pi bridge)"
	} else if r := remedyText(hint); r != "" {
		remedy = r + " (suggested by this target's manifest)"
	} else {
		remedy = "install the pi extension from this repo (pi install git:github.com/avivsinai/agent-message-queue)"
		if v := strings.TrimPrefix(Version, "v"); v != "" && v != "dev" {
			remedy = fmt.Sprintf("install the pi extension from this repo at the release tag (pi install git:github.com/avivsinai/agent-message-queue@v%s)", v)
		}
	}
	return fmt.Sprintf("the pi bridge extension for handle %q %s; this amq-remote needs bridge_revision %d or later: %s, then reload the pi session",
		handle, have, MinBridgeRevision, remedy)
}

// maxRemedyBytes bounds a supplied remedy. A longer one is rejected, not
// cut, so a truncated command is never shown.
const maxRemedyBytes = 256

// isDefaultIgnorable reports whether r is invisible in every rendering
// (U+200B zero width space, U+034F combining grapheme joiner, variation
// selectors). A remedy of only these carries no usable command and must
// fall through to the next remedy source, not suppress it.
func isDefaultIgnorable(r rune) bool {
	return unicode.Is(unicode.Cf, r) || r == 0x34F || r >= 0x200B && r <= 0x200F || r >= 0xFE00 && r <= 0xFE0F || r >= 0xE0100 && r <= 0xE01EF
}

// remedyText normalizes a supplied remedy: whitespace runs become one space,
// control characters, format characters (bidi overrides, zero-width) and
// other default-ignorable code points are removed, and text that is empty
// after that, or longer than maxRemedyBytes, is "".
func remedyText(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case isDefaultIgnorable(r):
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	if out := b.String(); len(out) <= maxRemedyBytes {
		return out
	}
	return ""
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
// (protocol: session generation and epoch) — until the first receipt Inspect publishes the `unpinned` sentinel.
func New(target, handle string, dir bridgeDir) (*Attachment, error) {
	if target == "" {
		return nil, fmt.Errorf("pi: target is required")
	}
	if handle == "" {
		return nil, fmt.Errorf("pi: handle is required")
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
	// Restart recovery: rebuild in-memory correlation from the durable
	// seam BEFORE any endpoint call. A recovered attachment never
	// redispatches: an existing request file plus any receipt/event answers
	// from history (protocol: adapter recovery and evidence).
	a.recover()
	return a, nil
}

// Factory builds a kind "pi" Attachment from a registry.FactoryConfig.
func Factory(_ context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	return build(cfg, piWire)
}

func build(cfg registry.FactoryConfig, names wireNames) (core.Attachment, error) {
	var c config
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &c); err != nil {
			return nil, fmt.Errorf("parse pi config: %w", err)
		}
	}
	if c.Handle == "" {
		return nil, fmt.Errorf("pi config: handle is required (the pi session handle)")
	}
	if err := fsq.ValidateHandle(c.Handle); err != nil {
		return nil, fmt.Errorf("pi config: invalid handle: %v", err)
	}
	dir := bridgeDir{dir: bridgePath(cfg.Root, c.Handle, names), names: names}
	if st, err := os.Stat(dir.dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("pi: bridge extension directory not found at %s (is the %s extension running for handle %q?)", dir.dir, names.dir, c.Handle)
	}
	a, err := New(cfg.Target, c.Handle, dir)
	if err != nil {
		return nil, err
	}
	a.upgradeHint = c.UpgradeHint
	return a, nil
}

func init() {
	registry.Register("pi", Factory)
}

// clientRef is the request identity: protocol.EncodeRef of the record key,
// exactly as the protocol's identity and layout section defines it. The extension keys its
// receipts and events by the same ref.
func clientRef(key requests.Key) string {
	return protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
}

// recover rebuilds runs from receipts/ + events/ (protocol: adapter recovery and evidence). Receipts are
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
	withReceipt := map[string]bool{}
	for _, rc := range a.dir.listReceipts() {
		withReceipt[rc.Ref] = true
		creatorHost, targetID, requestID, derr := protocol.DecodeRef(rc.Ref)
		if derr != nil {
			continue
		}
		key := requests.Key{CreatorHost: creatorHost, TargetID: targetID, RequestID: requestID}
		events, evErr := a.dir.readEvents(rc.Ref)
		seeds = append(seeds, seeded{key: key, rc: rc, events: events, evErr: evErr})
	}
	// A pre-delivery refusal (busy, generation, expired, invalid) writes no
	// receipt, so its only record is the event stream. Recovery binds it
	// as a typed rejection without admission evidence.
	type refusal struct {
		key    requests.Key
		events []event
	}
	var refusals []refusal
	for _, ref := range a.dir.listEventRefs() {
		if withReceipt[ref] {
			continue
		}
		creatorHost, targetID, requestID, derr := protocol.DecodeRef(ref)
		if derr != nil {
			continue
		}
		if rc, rcErr := a.dir.readReceipt(ref); rc != nil || rcErr != nil {
			continue // a receipt exists; Lookup reads it
		}
		events, evErr := a.dir.readEvents(ref)
		if evErr != nil || !refusedFirst(ref, events) {
			continue
		}
		refusals = append(refusals, refusal{key: requests.Key{CreatorHost: creatorHost, TargetID: targetID, RequestID: requestID}, events: events})
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
		r.gen = rc.SessionGeneration
		a.observeGenerationLocked(rc.SessionGeneration, rc.PID)
		if sd.evErr != nil {
			// A refused event stream (foreign protocol) is never "no
			// events" — bind with the refusal so Lookup surfaces it.
			r.eventsRefused = sd.evErr
		} else {
			a.applyEventsLocked(r, sd.events)
		}
	}
	for _, rf := range refusals {
		if _, ok := a.runs[rf.key]; !ok {
			a.bindRefusalLocked(rf.key, rf.events)
		}
	}
}

// lateBind runs the recovery scan for one unseen key WITHOUT the lock (9a:
// the seam reads happen outside a.mu), then binds under it. A receipt for a
// ref whose run is not yet in the map (published by a prior process, or
// seeded/published between calls) binds a wildcard-epoch run with the
// receipt-proven state — history never hides behind an in-memory miss (protocol: adapter recovery and evidence).
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
	switch {
	case rc != nil:
		events, evErr = a.dir.readEvents(rc.Ref)
	case rcErr == nil:
		// No receipt: a pre-delivery refusal lives only in the events.
		events, evErr = a.dir.readEvents(clientRef(key))
	}
	for _, ev := range events {
		if ev.Event == "interaction" {
			a.refreshLive() // the approval projection needs it
			break
		}
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
		r.gen = rc.SessionGeneration
		a.observeGenerationLocked(rc.SessionGeneration, rc.PID)
		if evErr != nil {
			// A refused event stream (foreign protocol) is never "no
			// events" — bind with the refusal so Lookup surfaces it.
			r.eventsRefused = evErr
		} else {
			a.applyEventsLocked(r, events)
		}
	case rcErr != nil:
		// Unreadable or foreign-protocol receipt (protocol: protocol string): bind unconfirmed with
		// the seam error so Lookup surfaces it instead of guessing it into
		// evidence — or silently ignoring a record the endpoint still holds.
		r := a.bindRunLocked(key, "")
		r.notFound = rcErr
	case evErr == nil && refusedFirst(clientRef(key), events):
		// No receipt, a validated refused terminal: a typed rejection
		// without admission evidence.
		a.bindRefusalLocked(key, events)
	}
	// Absent receipt and no refusal: leave unbound; Lookup answers unknown.
}

// bindRefusalLocked binds a ref whose only record is a refused terminal
// event. The run stays unconfirmed, so it never reads as admitted, and a
// historical generation refusal does not move the current pin.
func (a *Attachment) bindRefusalLocked(key requests.Key, events []event) {
	r := a.bindRunLocked(key, "") // recovered history: epoch wildcard
	epoch, pid := a.epoch, a.epochPID
	a.applyEventsLocked(r, events)
	a.epoch, a.epochPID = epoch, pid
}

// refusedFirst reports whether the ref's first terminal event is refused.
func refusedFirst(ref string, events []event) bool {
	for _, ev := range events {
		if ev.Ref != "" && ev.Ref != ref {
			continue
		}
		switch ev.Event {
		case "refused":
			return true
		case "completed", "failed", "cancelled", "uncertain":
			return false
		}
	}
	return false
}

// bindRunLocked registers one correlation slot.
func (a *Attachment) bindRunLocked(key requests.Key, epoch string) *run {
	ref := clientRef(key)
	r := &run{
		key:       key,
		ref:       ref,
		runID:     a.dir.names.runPrefix + ref,
		epoch:     epoch,
		state:     protocol.StateRunning,
		nativeRef: a.dir.names.dir + " " + ref,
	}
	a.runs[key] = r
	a.order = append(a.order, key)
	return r
}

// observeGenerationLocked applies the epoch rule: only receipts pin. Any
// valid, different generation replaces the current pin — the code does NOT
// order observations by receipt time (P2-A, review 816-r3): with two
// concurrent readers, a stale read can momentarily regress the pin. The
// regression is self-healing: the extension answers a stale hint with
// refused(generation), which unpins to the sentinel and lets the next
// receipt re-pin. An invalid generation string is never pinned.
func (a *Attachment) observeGenerationLocked(gen string, pid int) {
	if gen != "" && protocol.ValidEpoch(gen) && gen != a.epoch {
		a.epoch = gen
		a.epochPID = pid
	}
}

// dropStalePinLocked unpins when the live bridge proves the pinned
// generation ended: it runs in a different process than the one whose
// receipt pinned it (kill, crash, app restart), or it publishes a different
// generation (/new, reload, fork keep the pid).
// Without this the first submit carried a dead epoch_hint, which the
// extension refused. Liveness never PINS (protocol: session generation and epoch): the next submit goes out as
// first contact and its receipt pins the live generation.
func (a *Attachment) dropStalePinLocked(live livenessState) {
	if a.epoch == "" || !live.live {
		return
	}
	otherProcess := a.epochPID > 0 && live.pid > 0 && live.pid != a.epochPID
	otherGeneration := live.gen != "" && protocol.ValidEpoch(live.gen) && live.gen != a.epoch
	if otherProcess || otherGeneration {
		a.epoch = ""
		a.epochPID = 0
	}
}

// seamObservation is one run's freshly read durable evidence (9a: the FILE
// I/O happens outside a.mu; only this immutable snapshot crosses the lock).
type seamObservation struct {
	ref      string
	receipt  *receipt // nil = absent or already confirmed
	rcErr    error    // unreadable/foreign-protocol receipt (never guessed around)
	readRc   bool     // the receipt was read (receipt and rcErr are the answer)
	events   []event
	evErr    error // unreadable/foreign-protocol event stream (unreadable vs protocol-string refusal)
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
	needLive := false // an open approval's projection depends on liveness
	for _, k := range a.order {
		r, ok := a.runs[k]
		if !ok || r.acked {
			continue
		}
		needLive = needLive || len(r.open) > 0
		readRc := !r.confirmed
		// e3b (review-816-r6 P2-1): a run whose stream was REFUSED (foreign
		// protocol) must keep re-reading the events file even after
		// the run went terminal — otherwise an operator rewriting the seam
		// as clean v1 lines can never lift the refusal and every Lookup on
		// the run returns the refusal error instead of the terminal result.
		// Once a refusal is proven, only a PRESENT, protocol-validated
		// stream lifts it (applyObservationLocked keeps that gate); until
		// then a terminal run with eventsRefused stays in the read set.
		readEv := !r.terminal || r.eventsRefused != nil
		if readRc || readEv {
			queue = append(queue, pending{r: r, readRc: readRc, readEv: readEv})
		}
	}
	a.mu.Unlock()

	if len(queue) == 0 {
		if needLive {
			a.refreshLive()
		}
		return
	}
	obs := make(map[string]*seamObservation, len(queue))
	for _, p := range queue {
		o := &seamObservation{ref: p.r.ref}
		if p.readRc {
			rc, err := a.dir.readReceipt(p.r.ref)
			o.receipt, o.rcErr, o.readRc = rc, err, true
		}
		if p.readEv {
			evs, err := a.dir.readEvents(p.r.ref)
			o.events, o.evErr, o.readEvts = evs, err, true
			for _, ev := range evs {
				needLive = needLive || ev.Event == "interaction"
			}
		}
		obs[p.r.ref] = o
	}
	if needLive {
		a.refreshLive()
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

// refreshLive reads liveness outside a.mu and keeps it for the approval
// projection.
func (a *Attachment) refreshLive() livenessState {
	live := a.dir.liveness(a.now()) // 9a: FS read, no lock
	a.mu.Lock()
	a.live = live
	a.mu.Unlock()
	return live
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
			// NOT cleared here (review 816-r5 P1): the receipt and the event
			// log are different files — reading one proves nothing about the
			// other's protocol. A protocol-string refusal recorded before the receipt
			// landed (events-first ordering) must survive the receipt: only
			// a present, validated stream (the events branch below) lifts it.
			r.gen = o.receipt.SessionGeneration
			a.observeGenerationLocked(o.receipt.SessionGeneration, o.receipt.PID)
		case o.rcErr != nil && !r.confirmed:
			// Unreadable or foreign-protocol: never consume it as evidence.
			// Only an unconfirmed run records the error (review 816-r3
			// P2-B): a stale failed read landing after a successful receipt
			// must not resurrect notFound — consume() never re-reads a
			// confirmed run's receipt, so that would wedge the run into
			// permanent uncertainty.
			r.notFound = o.rcErr
		}
	} else if o.readRc && !r.confirmed {
		// A successful read shows the receipt is absent: an earlier read
		// error no longer blocks the evidence (a receiptless refusal has no
		// receipt to clear it).
		r.notFound = nil
	}
	if o.readEvts {
		switch {
		case o.evErr != nil:
			// Split: a rotated or temporarily unreadable log is
			// tolerated (keep state, retry next refresh), but a
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
			// exactly as they are, never clear, never re-apply.
			a.applyEventsLocked(r, o.events)
		}
	}
}

// applyEventsLocked applies the ref's event stream (protocol: events). The FIRST terminal
// event is final for the ref; later lines never overwrite it. A refused
// event maps to the endpoint's typed refusal codes: fire-time window expiry
// with a receipt present → expired (the receipt STAYS — admission
// happened, execution was refused), generation mismatch → stale_epoch,
// busy → busy, anything else → native_error.
func (a *Attachment) applyEventsLocked(r *run, events []event) {
	for _, ev := range events {
		if ev.Ref != "" && ev.Ref != r.ref {
			continue
		}
		if !r.terminal && terminalEvent(ev.Event) {
			// The run's end closes its open approvals first, so the
			// endpoint sees each resolution before the terminal outcome.
			a.endInteractionsLocked(r)
		}
		switch ev.Event {
		case "started":
			if !r.terminal {
				r.state = protocol.StateRunning
			}
		case "interaction":
			if !r.terminal {
				a.openInteractionLocked(r, ev)
			}
		case "interaction_resolved":
			if !r.terminal {
				a.resolveInteractionLocked(r, ev)
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
		case "uncertain":
			// The extension stopped tracking the ref without a native
			// outcome (restart, shutdown before start, or a follow-up that
			// never started). The run may or may not have happened, so the
			// record stays uncertain; it is never failed or completed.
			if r.terminal {
				break
			}
			r.terminal = true
			r.uncertain = true
			r.state = protocol.StateUncertain
			r.errText = ev.Error
		case "refused":
			if r.terminal {
				break
			}
			r.terminal = true
			r.state = protocol.StateRejected
			r.refused = refusalCodeFor(ev.Reason)
			r.refusedMsg = ev.Reason
			if ev.Error != "" {
				r.refusedMsg += ": " + ev.Error
			}
			if ev.Reason == "generation" {
				// The pinned generation is proven stale. The refused
				// event carries no live generation (the extension refused
				// pre-delivery and wrote no receipt), so the adapter drops
				// back to the `unpinned` sentinel; the next delivered
				// request's receipt re-pins the live generation.
				a.epoch = ""
				a.epochPID = 0
			}
		}
		if r.terminal {
			break // first-terminal-wins (protocol: events): later lines are not evidence
		}
	}
}

// terminalEvent reports whether an event type is terminal for its ref.
func terminalEvent(name string) bool {
	switch name {
	case "completed", "failed", "cancelled", "uncertain", "refused":
		return true
	}
	return false
}

// openInteractionLocked applies one interaction line (protocol: bridge
// revision 4). A line that is not a well-formed approval is ignored, so the
// local face answers it. The line lets a remote face approve only for
// presence remote, a prompt within MaxApprovalPreview, and an approve option
// it offers; otherwise a remote face may only reject. The remote options are
// only the approve and reject options, never a broader scope.
func (a *Attachment) openInteractionLocked(r *run, ev event) {
	id := ev.InteractionID
	if !validInteractionID(id) || r.interactions[id] != nil {
		return
	}
	expires, err := time.Parse(time.RFC3339Nano, ev.ExpiresAt)
	if ev.Kind != "approval" || ev.ManifestHash == "" || err != nil || !offers(ev.Options, ev.RejectOption) {
		return
	}
	prompt, cut := protocol.TruncateText(ev.Prompt, protocol.MaxApprovalPreview)
	oi := &openInteraction{id: id, prompt: prompt, reject: ev.RejectOption, manifestHash: ev.ManifestHash, expiresAt: expires}
	if ev.Presence == "remote" && !cut && ev.ApproveOption != ev.RejectOption && offers(ev.Options, ev.ApproveOption) {
		oi.approve = ev.ApproveOption
	}
	if r.interactions == nil {
		r.interactions = map[string]*openInteraction{}
	}
	r.interactions[id] = oi
	r.open = append(r.open, oi)
	if len(r.open) == 1 {
		a.emitQuestionLocked(r, oi)
	}
}

// resolveInteractionLocked applies one interaction_resolved line. The
// resolution is kept as evidence with its exact outcome and option. Only the
// approval the endpoint shows publishes it; an unknown outcome is ignored.
func (a *Attachment) resolveInteractionLocked(r *run, ev event) {
	switch protocol.ResolutionOutcome(ev.Outcome) {
	case protocol.ResolutionAnswered, protocol.ResolutionElsewhere, protocol.ResolutionRunEnded:
	default:
		return
	}
	a.closeInteractionLocked(r, ev.InteractionID, protocol.Resolution{
		InteractionID: ev.InteractionID, Outcome: protocol.ResolutionOutcome(ev.Outcome), Option: ev.Option,
	})
}

// endInteractionsLocked closes every open approval as run_ended when its
// run reaches a terminal event, uncertain included.
func (a *Attachment) endInteractionsLocked(r *run) {
	for len(r.open) > 0 {
		id := r.open[len(r.open)-1].id // queued ones first: only the head publishes
		a.closeInteractionLocked(r, id, protocol.Resolution{InteractionID: id, Outcome: protocol.ResolutionRunEnded})
	}
}

// closeInteractionLocked records how one open approval ended and, when it
// is the one the endpoint shows, publishes the resolution and the next one.
func (a *Attachment) closeInteractionLocked(r *run, id string, res protocol.Resolution) {
	idx := -1
	for i, oi := range r.open {
		if oi.id == id {
			idx = i
		}
	}
	if idx < 0 {
		return
	}
	r.open = append(r.open[:idx], r.open[idx+1:]...)
	if r.outcomes == nil {
		r.outcomes = map[string]protocol.Resolution{}
	}
	r.outcomes[id] = res
	if idx != 0 {
		return // a queued approval the endpoint never showed
	}
	a.emitLocked(core.NativeEvent{
		Type: core.EventQuestionResolved, Key: r.key, RunID: r.runID,
		Interaction: &protocol.Interaction{InteractionID: id},
		Remote:      res.Outcome == protocol.ResolutionAnswered, Outcome: res.Outcome, Option: res.Option,
	})
	if len(r.open) > 0 {
		a.emitQuestionLocked(r, r.open[0])
	}
}

// emitQuestionLocked publishes oi as the run's pending interaction.
func (a *Attachment) emitQuestionLocked(r *run, oi *openInteraction) {
	a.emitLocked(core.NativeEvent{Type: core.EventQuestion, Key: r.key, RunID: r.runID, Interaction: a.projectLocked(r, oi)})
}

// answerableLocked reports whether a remote answer can apply to oi now: a
// live bridge of revision 4 or later, in the generation of the ref's
// receipt, before the approval expires.
func (a *Attachment) answerableLocked(r *run, oi *openInteraction) bool {
	live := a.live
	return live.live && live.revision >= ApproveBridgeRevision && r.gen != "" && live.gen == r.gen && a.now().Before(oi.expiresAt)
}

// projectLocked is the endpoint's view of one open approval. RemoteAnswer
// and ApproveOption are set only while a remote answer can apply, so a
// surface never offers an answer the extension would not take.
func (a *Attachment) projectLocked(r *run, oi *openInteraction) *protocol.Interaction {
	in := &protocol.Interaction{InteractionID: oi.id, Kind: "approval", Prompt: oi.prompt, Options: []string{oi.reject}, RejectOption: oi.reject}
	if !a.answerableLocked(r, oi) {
		return in
	}
	in.RemoteAnswer = true
	if oi.approve != "" {
		in.ApproveOption = oi.approve
		in.Options = []string{oi.approve, oi.reject}
	}
	return in
}

// pendingLocked returns the endpoint's view of the run's pending
// interaction, or nil.
func (a *Attachment) pendingLocked(r *run) *protocol.Interaction {
	if len(r.open) == 0 {
		return nil
	}
	return a.projectLocked(r, r.open[0])
}

// offers reports whether option is non-empty and one of options.
func offers(options []string, option string) bool {
	if option == "" {
		return false
	}
	for _, o := range options {
		if o == option {
			return true
		}
	}
	return false
}

// refusalCodeFor maps a refused reason to the endpoint's typed codes.
func refusalCodeFor(reason string) protocol.Code {
	switch reason {
	case "expired":
		return protocol.CodeExpired
	case "generation":
		return protocol.CodeStaleEpoch
	case "busy":
		return protocol.CodeBusy
	case "revision":
		// The extension does not accept this adapter's bridge_revision.
		return protocol.CodeUnsupported
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

// emitLocked queues one event for subscribers. Callers hold a.mu. Delivery
// runs on one drainer goroutine outside the lock, in queue order, so a
// caller never blocks on a subscriber and the endpoint sees events in the
// order the seam produced them.
func (a *Attachment) emitLocked(ev core.NativeEvent) {
	if len(a.listeners) == 0 {
		return
	}
	a.emitq = append(a.emitq, ev)
	if !a.emitting {
		a.emitting = true
		go a.drainEmits()
	}
}

// drainEmits delivers queued events until the queue is empty.
func (a *Attachment) drainEmits() {
	for {
		a.mu.Lock()
		if len(a.emitq) == 0 {
			a.emitting = false
			a.mu.Unlock()
			return
		}
		ev := a.emitq[0]
		a.emitq = a.emitq[1:]
		fns := make([]func(core.NativeEvent), 0, len(a.listeners))
		for _, fn := range a.listeners {
			fns = append(fns, fn)
		}
		a.mu.Unlock()
		for _, fn := range fns {
			fn(ev)
		}
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
// generation, or the `unpinned` sentinel before the first receipt (protocol: session generation and epoch;
// an empty epoch would defeat stale-epoch protection because "" == "").
func (a *Attachment) Inspect() protocol.Session {
	s, _ := a.InspectSubmit()
	return s
}

// InspectSubmit implements core.SubmitBlocker: the session projection and,
// when it advertises submit false for an old bridge, the install and reload
// text, both from one liveness read.
func (a *Attachment) InspectSubmit() (protocol.Session, string) {
	a.consume()                     // 9a: seam reads outside a.mu; apply under it
	live := a.dir.liveness(a.now()) // 9a: FS read, no lock
	a.mu.Lock()
	a.dropStalePinLocked(live)
	var pending *string
	for _, k := range a.order {
		if r, ok := a.runs[k]; ok && !r.acked && len(r.open) > 0 {
			id := r.open[0].id
			pending = &id // the oldest bound run's approval waits longest
			break
		}
	}
	epoch := a.epoch
	if epoch == "" {
		// Before the first receipt the epoch names the live generation, so
		// a submit prepared against it cannot land in a later one.
		epoch = SentinelUnpinned
		if e := addressEpoch(live.gen); live.live && e != "" {
			epoch = e
		}
	}
	a.mu.Unlock()
	// A live bridge older than MinBridgeRevision gets no new submissions
	// (protocol: bridge revision). Offline, the revision is unknown and
	// Submit applies the same fence at dispatch.
	submit := !live.live || live.revision >= MinBridgeRevision
	blocked := ""
	if !submit {
		blocked = oldBridgeMessage(a.handle, live, a.upgradeHint)
	}
	att, status := "live", "idle"
	if !live.live {
		att, status = "offline", "offline"
	}
	// Tool approval needs a live bridge that raises interactions and reads
	// answers/ (protocol: bridge revision 4). A weaker bridge is refused the
	// capability, never given a substitute.
	approve := live.live && live.revision >= ApproveBridgeRevision
	return protocol.Session{
		Schema:             protocol.SchemaSession,
		TargetID:           a.target,
		Epoch:              epoch,
		Harness:            "pi",
		DisplayName:        "pi " + a.handle,
		Attachment:         att,
		Status:             status,
		PendingInteraction: pending,
		// Capability projection: Steer is FALSE (v1 is followUp-only; the
		// endpoint's D1 gate refuses deliver=steer pre-adapter and the
		// adapter refuses it again pre-side-effect), cancel/question are
		// false (no native seam; keystrokes are forbidden), approve needs
		// bridge revision 4, terminal is unavailable.
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: submit, CancelRequest: false, Steer: false,
			ApproveTool: approve, AnswerQuestion: false, Terminal: "unavailable",
		},
		// The adapter never claims `admitted` in Inspect — the per-ref
		// receipt is the admission proof, not a session-wide capability.
		Evidence:   &protocol.Evidence{Submit: protocol.EvidenceSubmitted, Completion: "run_terminal"},
		ObservedAt: protocol.FormatTime(a.now()),
	}, blocked
}

// Submit implements core.Attachment.
//
// Contract flow (protocol: requests; receipts): publish the request atomically and create-new
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
	// Epoch gate: once a receipt pinned a generation, the caller's epoch
	// must match it exactly. While unpinned (a.epoch == "") the caller's
	// epoch must be the address epoch `unpinned.<generation>` it read from
	// Inspect; that generation is the hint, never a later liveness read.
	// The endpoint's admissible check compares cmd.Epoch against
	// Inspect().Epoch, so these only fire on a race; they mirror the
	// stale-epoch refusal positively either way.
	firstContact := a.epoch == ""
	epochHint := a.epoch
	if !firstContact && req.Epoch != a.epoch {
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeStaleEpoch, Message: "epoch does not match the pinned session generation; re-inspect"}, nil
	}
	if firstContact {
		gen, ok := addressedGeneration(req.Epoch)
		if !ok {
			a.mu.Unlock()
			return core.Admission{Code: protocol.CodeStaleEpoch, Message: "epoch addresses no pi session generation; re-inspect"}, nil
		}
		epochHint = gen
	}
	// The hint is captured in the SAME critical section as the gate
	// above (review 816-r3 P1): re-reading it after the lock-free liveness
	// check let a concurrent re-pin/refuse publish a generation the gate
	// never validated — or swap it for the live generation on a
	// refused(generation), addressing a session the gate never checked.
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
		if r.refused != "" {
			adm := refusalAdmission(r)
			a.mu.Unlock()
			return adm, nil
		}
		a.mu.Unlock()
		// 9a: liveness is a filesystem read — take it without the lock.
		live := a.dir.liveness(a.now())
		if !live.live {
			// No receipt + stale/absent liveness. The request file
			// stays for a later bridge (the adapter owns it and never
			// deletes); delivery is still possible, so this is an
			// UNCERTAIN-shaped refusal — the endpoint keeps the correlation
			// (admissionCause maps any native error to attachment_lost).
			return core.Admission{}, protocol.Refuse(protocol.CodeAttachmentLost,
				"no live pi bridge for handle %q (bridge.liveness %s); request %s stays for a later bridge", a.handle, live.reason, r.ref)
		}
		return core.Admission{RunID: rid}, fmt.Errorf("pi: receipt for %s not yet observed; submission uncertain", r.ref)
	}
	if req.Input.Deliver == protocol.DeliverSteer {
		// v1 is followUp-only. The endpoint's D1 gate already refuses
		// deliver=steer; this is the belt-and-braces adapter-side refusal,
		// before any file write.
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeUnsupported, Message: "deliver=steer is disabled in v1 (pi advertises no Steer); use deliver=turn"}, nil
	}
	// Fresh submit: the liveness gate is PRE-SIDE-EFFECT — nobody listening
	// means nothing is written and the refusal is positive (protocol: receipts). 9a: the
	// filesystem read happens without the lock.
	a.mu.Unlock()
	live := a.dir.liveness(a.now())
	if !live.live {
		return core.Admission{Code: protocol.CodeAttachmentLost, Message: fmt.Sprintf("no live pi bridge for handle %q (bridge.liveness %s)", a.handle, live.reason)}, nil
	}
	if live.revision < MinBridgeRevision {
		// A missing or lower revision is never read as compatible: the
		// extension lacks the rules this adapter's evidence relies on.
		return core.Admission{Code: protocol.CodeUnsupported, Message: oldBridgeMessage(a.handle, live, a.upgradeHint)}, nil
	}
	if firstContact {
		// First contact: the request addresses the generation the caller
		// observed at Inspect. A live bridge in another generation means
		// the session changed (/new, reload, restart), so nothing is
		// published. This addresses the session only; it is not admission
		// evidence and never pins (only receipts pin).
		if !protocol.ValidEpoch(live.gen) {
			return core.Admission{Code: protocol.CodeAttachmentLost, Message: fmt.Sprintf("live pi bridge for handle %q publishes no valid session_generation", a.handle)}, nil
		}
		if live.gen != epochHint {
			return core.Admission{Code: protocol.CodeStaleEpoch, Message: "the pi session changed after inspect; re-inspect"}, nil
		}
	}
	// File I/O outside a.mu: the mutex guards correlation state, not the
	// seam. A concurrent same-key submit cannot happen (the endpoint's
	// per-runtime reservation serializes dispatches), and a bind after the
	// write below re-checks the map under the lock.
	ref := clientRef(req.Key)
	preq := deliverRequest{
		Ref:            ref,
		Text:           req.Input.Text,
		DeliverAs:      "followUp", // v1 delivers followUp only
		NotAfter:       req.NotAfter,
		EpochHint:      epochHint,
		CreatedAt:      protocol.FormatTime(a.now()),
		BridgeRevision: MinBridgeRevision,
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
			// rewrite (O_EXCL; protocol: requests), classify from the durable seam.
			o := a.readSeamFor(refOfRun) // 9a: FS reads without the lock
			a.mu.Lock()
			a.applyObservationLocked(r, o)
			rid := r.runID
			if r.confirmed {
				a.mu.Unlock()
				return core.Admission{Admitted: true, RunID: rid}, nil
			}
			if r.refused != "" {
				adm := refusalAdmission(r)
				a.mu.Unlock()
				return adm, nil
			}
			a.mu.Unlock()
			live := a.dir.liveness(a.now())
			if !live.live {
				// UNCERTAIN-shaped: the file is already published, a later
				// bridge can still deliver; admissionCause keeps the
				// correlation (never a terminal rejected record).
				return core.Admission{}, protocol.Refuse(protocol.CodeAttachmentLost,
					"no live pi bridge for handle %q (bridge.liveness %s); request %s stays for a later bridge", a.handle, live.reason, refOfRun)
			}
			return core.Admission{RunID: rid}, fmt.Errorf("pi: receipt for %s not yet observed; submission uncertain", refOfRun)
		}
		// Pre-send/ambiguous seam failure: nothing provably reached the
		// extension. Return the error so the endpoint records uncertain —
		// the send primitive cannot report WHY, so a refusal here would
		// guess (the text may still be delivered by a later bridge scan).
		return core.Admission{}, err
	}

	// Receipt poll: the extension writes the receipt when it delivers (protocol: receipts).
	// The window is REAL wall time (how long this call may block the
	// endpoint), deliberately not the frozen test clock — a.now stays for
	// timestamps and liveness freshness. 9a: each poll step reads the seam
	// WITHOUT the lock, then re-locks to apply.
	deadline := time.Now().Add(a.submitWait)
	for {
		o := a.readSeamFor(refOfRun)
		a.mu.Lock()
		a.applyObservationLocked(r, o)
		confirmed, refused := r.confirmed, r.refused != ""
		var adm core.Admission
		if refused {
			adm = refusalAdmission(r)
		}
		a.mu.Unlock()
		if confirmed {
			return core.Admission{Admitted: true, RunID: rid}, nil
		}
		if refused {
			return adm, nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(submitPollStep)
	}
	live = a.dir.liveness(a.now())
	if !live.live {
		// The bridge died mid-window (the file is already published; the
		// file stays for a later bridge). Delivery is
		// still possible from a later bridge scan, so the refusal is
		// UNCERTAIN-shaped (admissionCause keeps the correlation).
		return core.Admission{}, protocol.Refuse(protocol.CodeAttachmentLost,
			"no live pi bridge for handle %q (bridge.liveness %s); request %s stays for a later bridge", a.handle, live.reason, refOfRun)
	}
	return core.Admission{RunID: rid}, fmt.Errorf("pi: receipt for %s not yet observed; submission uncertain", refOfRun)
}

// refusalAdmission is the typed refusal for a run whose stream holds a
// definitive pre-delivery refusal and no receipt: the extension never
// delivers that ref, so the refusal is final. Every Submit path returns it
// before the missing-receipt fallback. Called under a.mu.
func refusalAdmission(r *run) core.Admission {
	return core.Admission{Code: r.refused, Message: "pi bridge refused the request: " + r.refusedMsg}
}

// readSeamFor reads one run's durable evidence by ref WITHOUT the lock
// (9a): a single-run observation for the Submit poll and retry paths.
func (a *Attachment) readSeamFor(ref string) *seamObservation {
	o := &seamObservation{ref: ref}
	o.receipt, o.rcErr = a.dir.readReceipt(ref)
	o.readRc = true
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
		// The event stream was refused (foreign protocol). Surface the
		// refusal — it must never silently read as "no events".
		return core.Evidence{}, r.eventsRefused
	}
	ev := core.Evidence{Known: true, RunID: r.runID, State: r.state, Interaction: a.pendingLocked(r)}
	if r.acked {
		// Result released: nothing retained, admission proven.
		ev.Class = core.EvidenceNone
		ev.Admitted = true
		return ev, nil
	}
	if r.refused != "" {
		// Definitive native refusal. With a receipt present the
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
	if r.uncertain {
		// No native outcome exists for the ref: admission may be proven,
		// but completion is not, so the endpoint keeps the record uncertain.
		ev.Class = core.EvidenceUnknown
		ev.State = protocol.StateUncertain
		return ev, nil
	}
	if r.terminal {
		// A terminal turn event proves the turn ran — admission happened
		// regardless of whether the receipt file survives (rotation tolerance).
		ev.Class = core.EvidenceHistoryTerminated
		ev.Admitted = true
		ev.Result = r.result()
		return ev, nil
	}
	if r.confirmed {
		// Receipt present, no terminal event (rotated/absent log
		// included) → accepted, confirmed-running. NEVER uncertain: the
		// receipt is positive admission evidence.
		ev.Class = core.EvidenceConfirmed
		ev.Admitted = true
		return ev, nil
	}
	// No receipt: uncertain (protocol: adapter recovery and evidence). The record keeps correlating.
	ev.Class = core.EvidenceUnknown
	return ev, nil
}

// CancelExact implements core.Attachment. There is no native cancel seam
// (keystrokes are forbidden; protocol: capabilities): a cancel is intent only and resolved when
// the run is already terminal.
func (a *Attachment) CancelExact(key requests.Key, epoch string) (core.CancelEvidence, error) {
	a.consume()     // 9a: seam reads outside a.mu; apply under it
	a.lateBind(key) // 9a: same, for a key not yet bound
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.runs[key]
	if !ok {
		return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "pi adapter has no native cancel seam"}, nil
	}
	if r.epoch != "" && r.epoch != epoch {
		return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "epoch mismatch"}, nil
	}
	if r.state.Terminal() {
		return core.CancelEvidence{Disposition: protocol.CancelNoopTerminal}, nil
	}
	return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "pi adapter has no native cancel seam; the run resolves from the bridge event stream"}, nil
}

// Respond implements core.Attachment for tool approvals (protocol: bridge
// revision 4). An answer already on disk with the same identity is
// delivered, whatever the bridge state is now. A fresh answer is written only
// for the run's pending interaction, with an offered option, while a remote
// answer can apply; a revision-3 bridge keeps already_resolved. The answer
// file carries the manifest hash from the interaction line, so the extension
// applies it to exactly that tool call. The endpoint owns first-answer-wins;
// the extension reports which answer applied through interaction_resolved.
func (a *Attachment) Respond(key requests.Key, epoch, interactionID, option string) (protocol.Code, error) {
	a.consume()     // 9a: seam reads outside a.mu; apply under it
	a.lateBind(key) // 9a: same, for a key not yet bound
	a.mu.Lock()
	r, ok := a.runs[key]
	var oi *openInteraction
	if ok && (r.epoch == "" || r.epoch == epoch) {
		oi = r.interactions[interactionID]
	}
	if oi == nil {
		a.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	}
	ans := answerRecord{
		Protocol: a.dir.names.protocol, Ref: r.ref, InteractionID: interactionID,
		ManifestHash: oi.manifestHash, Option: option, At: protocol.FormatTime(a.now()),
	}
	a.mu.Unlock()
	// Recognizing a publication is not authorizing a new one: a replay of
	// the answer on disk is delivered before any gate for a fresh write.
	if done, err := a.dir.answerPublished(ans); err != nil || done {
		return "", err
	}
	live := a.refreshLive()
	a.mu.Lock()
	switch {
	case !live.live:
		a.mu.Unlock()
		return protocol.CodeAttachmentLost, nil
	case live.revision < ApproveBridgeRevision || len(r.open) == 0 || r.open[0] != oi:
		a.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	case !a.now().Before(oi.expiresAt):
		a.mu.Unlock()
		return protocol.CodeExpired, nil
	case !a.answerableLocked(r, oi):
		// The session that raised the approval is gone.
		a.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	case !offers(a.projectLocked(r, oi).Options, option):
		a.mu.Unlock()
		return protocol.CodeInvalid, nil
	}
	a.mu.Unlock()
	err := a.dir.publishAnswer(ans) // file I/O outside a.mu
	if errors.Is(err, ErrAlreadyDelivered) {
		// Another writer published first: the same answer is delivered,
		// any other answer on disk stands as the first.
		done, rerr := a.dir.answerPublished(ans)
		if rerr != nil {
			return "", rerr
		}
		if done {
			return "", nil
		}
		return protocol.CodeAlreadyResolved, nil
	}
	if err != nil {
		return "", err
	}
	return "", nil
}

// ResolvedInteraction implements core.InteractionResolver: how one approval
// of the key's run ended, as the event stream recorded it.
func (a *Attachment) ResolvedInteraction(key requests.Key, epoch, interactionID string) (protocol.Resolution, bool) {
	a.consume()     // 9a: seam reads outside a.mu; apply under it
	a.lateBind(key) // 9a: same, for a key not yet bound
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.runs[key]
	if !ok || (r.epoch != "" && r.epoch != epoch) {
		return protocol.Resolution{}, false
	}
	res, done := r.outcomes[interactionID]
	return res, done
}

// SetNow replaces the attachment's clock, which dates answers, decides
// approval expiry, and judges liveness freshness. For in-process harnesses
// and tests; production uses the wall clock.
func (a *Attachment) SetNow(now func() time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = now
}

// NativeSessionID implements core.NativeIdentifier: the live bridge's pi
// session id (protocol: bridge revision 4), which a relay share pins. It is
// "" without a live bridge or a valid session_id, so the share is refused.
func (a *Attachment) NativeSessionID() string {
	live := a.dir.liveness(a.now())
	if !live.live || !validSessionID(live.sessionID) {
		return ""
	}
	return live.sessionID
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
		// The endpoint unsubscribes under its own lock, which delivery
		// takes, so this cannot wait for the queue. Events still queued
		// when the last subscriber leaves are dropped; Lookup evidence
		// (the pending interaction and every resolution) carries the same
		// state for the next reconcile.
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.listeners, id)
		if len(a.listeners) == 0 {
			a.emitq = nil
		}
	}
}
