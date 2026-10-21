// Package core is the amq-remote endpoint: it owns request identity and
// records, runs the admission algorithm against native attachments, applies
// native evidence, reconciles after a restart, and publishes revisions. It
// does not know how commands arrive; carriers decode with protocol and call
// Handle.
package core

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Source is the authenticated origin of a command. Host is the peer host from
// the AMQ carrier or LocalHost for the CLI; it is never a claimed handle.
type Source struct {
	Host string
	// Origin is carrier routing kept on the record so publication can find
	// its way back after a restart. It carries no authority.
	Origin map[string]string
	// Shared is the target ids shared with this source. Nil means unfiltered
	// (local socket, mailbox, Buzz). A link source always carries a non-nil
	// list, possibly empty: session list and inspect show only these targets.
	Shared []string
	// NativeSession and Credential come only from a link's verified signed
	// submit: the native session the user signed for and the consent key
	// that signed. Core stores both on the record and checks them again right
	// before the handoff to the harness.
	NativeSession string
	Credential    string
}

// ConsentCheck reports whether the consent key credential of the link with
// creator host is still accepted. It runs under the endpoint lock, so it
// reads an immutable snapshot and never waits.
type ConsentCheck func(creatorHost, credential string) bool

// CarrierLink is the Origin "carrier" value of a link source: a server the
// user's machine dialed. Its Host is the link's creator host, link-<id>.
const CarrierLink = "link"

// OriginSink is the Origin key naming the link sink (its creator host) every
// record the link creates is published to.
const OriginSink = "sink"

// linkHostPrefix starts every creator host a link carrier uses.
const linkHostPrefix = "link-"

// LocalHost is the source host the local IPC socket stamps on every command
// it carries, and so the creator host of every request submitted over it.
// No carrier derives it: the AMQ carrier's hosts start "amq:" and the Buzz
// carrier's "buzz-".
const LocalHost = "local"

// Publisher receives every new revision with the record's carrier origin. A
// returned error leaves the revision unpublished; Reconcile republishes it
// later. Republishing the same revision is safe: receivers upsert by request
// and revision.
type Publisher func(protocol.Snapshot, map[string]string) error

// CrashPoint is a test hook that may return an error at a named boundary to
// simulate a process death there. Production endpoints leave it nil.
type CrashPoint func(point string) error

// ErrCrashed is returned by Handle when the crash hook fired.
var ErrCrashed = errors.New("endpoint crashed at boundary")

// ErrDrainIncomplete is returned by Close when the bounded shutdown drain
// ended with publication obligations it could not discharge: a chained
// revision was skipped (its attempt failed or the store was closed before it
// could be adopted) or handlers were still in flight at the drain timeout.
// The durable unpublished state is preserved for Reconcile recovery on the
// next start; Close does NOT report the shutdown as clean.
var ErrDrainIncomplete = errors.New("drain incomplete")

// Crash boundary names, in the order the design lists them.
const (
	PointBeforeReceived    = "before:received_commit"
	PointAfterReceived     = "after:received_commit"
	PointBeforeDispatching = "before:dispatching_commit"
	PointAfterDispatching  = "after:dispatching_commit"
	PointBeforeNative      = "before:native_submit"
	PointAfterNative       = "after:native_submit"
	PointBeforeResult      = "before:result_commit"
	PointAfterResult       = "after:result_commit"
	PointBeforeAck         = "before:native_ack"
	PointAfterAck          = "after:native_ack"
	PointBeforePublish     = "before:publish"
	PointAfterPublish      = "after:publish"
	PointBeforePublished   = "before:published_revision_commit"
)

type target struct {
	att         Attachment
	unsubscribe func()
}

// lifecycleState is the endpoint's phase: accepting commands, draining
// in-flight handlers before Close, or closed. The state is read+written
// under e.mu; the drain wait uses e.drained (a condition variable) so
// Close blocks until the last in-flight handler exits.
type lifecycleState int

const (
	stateAccepting lifecycleState = iota
	stateDraining
	stateClosed
)

// drainTimeout is the bound on how long Close waits for in-flight handlers.
// It is generous (30s) because a single handler's worst case is one native
// RPC (20s timeout in the codex adapter) plus a commit. If a handler has
// not returned by then, it is wedged and the endpoint closes anyway — the
// store's closed flag rejects further mutations from the stale handler.
const drainTimeout = 30 * time.Second

// Endpoint is the request handler. One Endpoint owns one Store.
type Endpoint struct {
	mu             sync.Mutex
	store          *requests.Store
	targets        map[string]*target
	publish        Publisher
	crash          CrashPoint
	now            func() time.Time
	observers      []func(*requests.Record)
	changed        chan struct{}
	compactHorizon time.Duration
	lastCompact    time.Time

	// visible records revisions the publisher has already delivered whose
	// published_revision marker did not commit. Reconcile then retries the
	// MARKER, never the delivery: the message is in the caller's mailbox and
	// re-delivery after consumption is a duplicate. In memory only: a process
	// crash in that window re-delivers, the documented exception.
	//
	// Visible means CONFIRMED DELIVERY awaiting its marker, NOT an
	// in-flight attempt. A separate `publishing` set owns in-flight attempts
	// so a failed publish cannot be mistaken for a delivered revision, and a
	// concurrent caller cannot clear a reservation it does not own.
	visible map[requests.Key]int64
	// publishing records keys with an in-flight publish attempt. Only one
	// publish per key runs at a time; concurrent callers for the same key see
	// it here and leave the work pending for the next Reconcile/Tick. The
	// owner clears only its own entry on completion.
	publishing map[requests.Key]bool
	// pubPending records accepted-but-skipped publication requests per key:
	// a publishLocked caller that hit publishing[key] coalesces its revision
	// here (highest requested revision wins). The finishing publisher of that
	// key — the single per-key owner — adopts the pending revision and
	// republishes it BEFORE releasing its drain obligation, so every accepted
	// revision is covered by exactly one obligation (its own in-flight
	// attempt or the owner's chained attempt) and Close's drain waits for the
	// whole chain.
	pubPending map[requests.Key]int64
	// failed holds the visible projection of a request whose async durable
	// write failed. Reads serve it so the owner sees uncertain, not a stale
	// running. It ends at the next durable write of the key, or
	// when a Reconcile pass over the key returns without error and no newer
	// failure was set meanwhile (failSeq tells them apart).
	failed  map[requests.Key]failure
	failSeq int64
	// Lifecycle: state transitions accepting -> draining -> closed.
	// inFlight counts handlers between entry (registerInFlight) and exit
	// (releaseInFlight). drained is a condition variable Close waits on.
	// The entry check + increment happen in ONE critical section so a
	// transition to draining cannot slip a handler in after the gate.
	state    lifecycleState
	inFlight int
	drained  *sync.Cond
	drainTO  time.Duration
	// drainObligations records, under e.mu, the highest undischarged pending
	// publication revision for a key whose chained attempt was skipped (its
	// continuation read failed, or the endpoint closed before adoption). It is
	// per-key/per-revision, not a global latch: a later successful publication
	// of that revision (or a strictly newer one) retires the entry, so Close
	// reports an incomplete drain ONLY for obligations genuinely left
	// unpublished. A failed attempt of attemptRev itself is NOT an
	// obligation: the attempt ran and was counted; the durable
	// unpublished state persists for Reconcile recovery.
	drainObligations map[requests.Key]int64
	// answering serializes the answers to one interaction of one request:
	// respond holds its lock from the first read through the refusal
	// rollback, so a rollback never erases a later answer with another
	// option. A rollback undoes only the intent its own respond recorded: a
	// replay of an earlier intent keeps it. An entry lives while a respond
	// holds or waits for it.
	answering map[answerKey]*answerLock
	consent   ConsentCheck
	// threadApprovals are the open approvals of turns no request owns, by
	// id, and threadSink is where they are shown.
	threadApprovals map[string]*threadApproval
	threadSink      ThreadApprovalSink
	// threadOwed are thread approval publications that failed, by id.
	threadOwed map[string]ThreadApproval
	// threadPubMu orders thread approval publications: it is taken before
	// e.mu and held from a state change through its publication.
	threadPubMu sync.Mutex
}

// Config configures New.
type Config struct {
	Store   *requests.Store
	Publish Publisher
	Crash   CrashPoint
	Now     func() time.Time
	// CompactHorizon is the age at which terminal, settled records become
	// eligible for compaction. Zero (default) disables compaction.
	CompactHorizon time.Duration
	// DrainTimeout overrides the default Close drain timeout. Tests use a
	// short value to prove the bound without sleeping 30s.
	DrainTimeout time.Duration
	// Consent checks a signed request's consent key at the handoff. Nil
	// refuses every signed request at the handoff.
	Consent ConsentCheck
}

// New builds an endpoint over an open store. Attachments register through
// Register; Reconcile should run before the first command.
func New(cfg Config) *Endpoint {
	e := &Endpoint{
		store:            cfg.Store,
		targets:          map[string]*target{},
		publish:          cfg.Publish,
		crash:            cfg.Crash,
		now:              cfg.Now,
		changed:          make(chan struct{}),
		compactHorizon:   cfg.CompactHorizon,
		visible:          map[requests.Key]int64{},
		publishing:       map[requests.Key]bool{},
		pubPending:       map[requests.Key]int64{},
		failed:           map[requests.Key]failure{},
		drainObligations: map[requests.Key]int64{},
		answering:        map[answerKey]*answerLock{},
		threadApprovals:  map[string]*threadApproval{},
		threadOwed:       map[string]ThreadApproval{},
		consent:          cfg.Consent,
		state:            stateAccepting,
		drainTO:          drainTimeout,
	}
	if cfg.DrainTimeout > 0 {
		e.drainTO = cfg.DrainTimeout
	}
	e.drained = sync.NewCond(&e.mu)
	if e.now == nil {
		e.now = time.Now
	}
	if e.publish == nil {
		e.publish = func(protocol.Snapshot, map[string]string) error { return nil }
	}
	return e
}

// SetConsent installs the consent check signed requests pass at the handoff.
// Until it is set, a deferred signed request keeps waiting rather than being
// refused, so startup reconciliation never ends one for lack of a check.
func (e *Endpoint) SetConsent(c ConsentCheck) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consent = c
}

// SetPublish replaces the publisher after construction. Serve uses this
// to break the circular dependency between the carrier and the endpoint:
// openServeStore creates the endpoint with a nil publish, then serve wires
// the carrier in once it exists.
func (e *Endpoint) SetPublish(p Publisher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p == nil {
		p = func(protocol.Snapshot, map[string]string) error { return nil }
	}
	e.publish = p
}

// Observe registers a callback for every record write and every storage
// failure projection. Tests use it to collect state history.
func (e *Endpoint) Observe(fn func(*requests.Record)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observers = append(e.observers, fn)
}

// Register attaches a runtime under its target id and subscribes to its
// native events. Registering the same target again replaces the attachment.
func (e *Endpoint) Register(att Attachment) {
	s := att.Inspect()
	e.mu.Lock()
	defer e.mu.Unlock()
	if old, ok := e.targets[s.TargetID]; ok && old.unsubscribe != nil {
		old.unsubscribe()
	}
	t := &target{att: att}
	t.unsubscribe = att.Subscribe(func(ev NativeEvent) { e.onNative(s.TargetID, ev) })
	e.targets[s.TargetID] = t
}

// UnregisterAll unsubscribes every attachment without closing the store:
// reconcile tests use it to drop a target mid-flight (the restart shape).
func (e *Endpoint) UnregisterAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, t := range e.targets {
		if t.unsubscribe != nil {
			t.unsubscribe()
		}
		delete(e.targets, id)
	}
}

// Close transitions the endpoint to draining, waits for in-flight handlers
// to finish (bounded by drainTimeout), then unsubscribes attachments and
// closes the store. A handler that never returns is abandoned: after the
// timeout, Close proceeds anyway and the store's closed flag rejects any
// further mutation the stale handler attempts. Returns an error naming how
// many handlers were still in flight if the bound was exceeded.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	if e.state == stateClosed {
		e.mu.Unlock()
		return nil
	}
	e.state = stateDraining
	// Wait for in-flight handlers to drain, bounded by drainTO. The timer
	// sets a flag and broadcasts so the loop exits even if inFlight > 0.
	if e.inFlight > 0 {
		timedOut := false
		timer := time.AfterFunc(e.drainTO, func() {
			e.mu.Lock()
			timedOut = true
			e.drained.Broadcast()
			e.mu.Unlock()
		})
		defer timer.Stop()
		for e.inFlight > 0 && !timedOut {
			e.drained.Wait()
		}
	}
	stale := e.inFlight
	e.state = stateClosed
	for id, t := range e.targets {
		if t.unsubscribe != nil {
			t.unsubscribe()
		}
		delete(e.targets, id)
	}
	// Report undischarged publication obligations ONCE, then clear them:
	// Close is terminal, so the durable unpublished state is left for
	// Reconcile recovery on the next start regardless. The map is per-key so
	// a later successful publication (this Close path or a concurrent
	// Reconcile) that already retired an entry does not get re-reported.
	skipped := len(e.drainObligations)
	e.drainObligations = map[requests.Key]int64{}
	e.mu.Unlock()
	err := e.store.Close()
	if stale > 0 {
		return fmt.Errorf("endpoint closed with %d handler(s) still in flight after %s drain timeout", stale, e.drainTO)
	}
	if skipped > 0 {
		if err != nil {
			return fmt.Errorf("%w (store close error: %v)", ErrDrainIncomplete, err)
		}
		return fmt.Errorf("%w: accepted revision(s) left unpublished (durable state preserved for Reconcile recovery)", ErrDrainIncomplete)
	}
	return err
}

// releaseInFlight decrements the in-flight count and wakes Close if it was
// waiting for handlers to drain. Called by Handle's defer.
func (e *Endpoint) releaseInFlight() {
	e.mu.Lock()
	e.inFlight--
	if e.inFlight == 0 && e.state == stateDraining {
		e.drained.Broadcast()
	}
	e.mu.Unlock()
}

// beginReconcile gates Reconcile/Tick on the lifecycle state and registers the
// sweep as in-flight in ONE critical section, mirroring Handle's entry gate.
// Without this, a Tick racing Close could start new native work
// (admitDeferred -> att.Submit, reconcileCancelRetry -> att.CancelExact,
// ack replay -> att.AcknowledgeResult) after the endpoint began draining, and
// Close, waiting only on Handle's inFlight, would return with that work
// unfinished. Tick refuses to start once draining has begun, and when it does
// start it is counted in inFlight so Close's drain wait bounds it. The caller
// MUST defer releaseInFlight when ok is true.
func (e *Endpoint) beginReconcile() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != stateAccepting {
		return false
	}
	e.inFlight++
	return true
}

// InFlight returns the current number of in-flight handlers. Test seam for
// observing the drain state without a sleep.
func (e *Endpoint) InFlight() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inFlight
}

// IsDraining reports whether the endpoint has begun shutdown (state is
// draining or closed). Test seam for synchronizing tests on the actual
// lifecycle transition instead of an elapsed-time sleep.
func (e *Endpoint) IsDraining() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state != stateAccepting
}

// IsDrainingState reports whether the endpoint is in the DRAINING state
// specifically (not closed). This distinguishes 'Close is waiting on
// in-flight handlers' from 'Close already set stateClosed and is past the
// drain wait', which IsDraining conflates. Test seam for the
// close-wait ordering regression: a test that releases a held publish after
// observing IsDraining()==true must fail if what it actually observed was
// stateClosed (Close raced past the drain without waiting).
func (e *Endpoint) IsDrainingState() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == stateDraining
}

// Handle runs one validated command from an authenticated source and returns
// the reply document: a Snapshot for request.* and interaction.respond, a
// Session or []Session for session.*.
func (e *Endpoint) Handle(cmd *protocol.Command, src Source) (any, error) {
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	if src.Host == "" {
		return nil, protocol.Refuse(protocol.CodeInvalid, "command source host is required")
	}
	// Lifecycle: check state and register as in-flight in ONE critical
	// section. A command arriving during draining/closed is refused with an
	// action-required code (the caller retries) — never a failure code that
	// would be recorded as the command's outcome.
	e.mu.Lock()
	if e.state != stateAccepting {
		e.mu.Unlock()
		return nil, protocol.Refuse(protocol.CodeDraining, "endpoint is shutting down; retry the command after restart")
	}
	e.inFlight++
	e.mu.Unlock()
	defer e.releaseInFlight()

	switch cmd.Op {
	case protocol.OpRequestSubmit:
		return e.submit(cmd, src)
	case protocol.OpRequestGet:
		return e.get(cmd, src)
	case protocol.OpRequestCancel:
		return e.cancel(cmd, src)
	case protocol.OpSessionList:
		return e.listFor(src), nil
	case protocol.OpSessionInspect:
		return e.inspectFor(cmd.TargetID, src)
	case protocol.OpSessionEvents:
		if isLink(src) {
			return nil, protocol.Refuse(protocol.CodeUnsupported, "session events are not offered over a link")
		}
		return e.inspect(cmd.TargetID)
	case protocol.OpInteractionRespond:
		return e.respond(cmd, src)
	}
	return nil, protocol.Refuse(protocol.CodeInvalid, "unknown op")
}

func (e *Endpoint) crashAt(point string) error {
	if e.crash == nil {
		return nil
	}
	if err := e.crash(point); err != nil {
		return fmt.Errorf("%w %s: %w", ErrCrashed, point, err)
	}
	return nil
}
