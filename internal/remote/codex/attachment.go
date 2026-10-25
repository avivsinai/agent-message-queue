package codex

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	"github.com/avivsinai/agent-message-queue/internal/remote/secretscan"
)

// ClientName is what the attachment reports to the app-server.
const ClientName = "amq-remote"

// Version is stamped by the binary.
var Version = "dev"

// confirmTimeout bounds how long Submit waits for our own userMessage item to
// confirm an idle turn/start actually accepted our text. The clientId echo is
// schema-backed but unverified live (quota-blocked probe), so an unconfirmed
// turn is refused, never admitted.
const confirmTimeout = 12 * time.Second

// Approval methods the app-server sends as server requests. The DM answers
// the decision approvals and shows the requests below them, which only the
// terminal answers; the rest stay local-only.
const (
	methodCommandApproval    = "item/commandExecution/requestApproval"
	methodFileChangeApproval = "item/fileChange/requestApproval"
	methodLegacyExecApproval = "execCommand/approval"
	// Requests answered with a shape other than {"decision": option}. The
	// DM shows them, but only the terminal answers them.
	methodPermissionsApproval = "item/permissions/requestApproval"
	methodToolUserInput       = "item/tool/requestUserInput"
	methodMcpElicitation      = "mcpServer/elicitation/request"
	// methodServerRequestResolved tells every client on the thread that a
	// server request was answered, by whichever client answered first.
	methodServerRequestResolved = "serverRequest/resolved"
)

type run struct {
	key          requests.Key
	epoch        string
	turnID       string
	state        protocol.State
	text         strings.Builder
	errText      string
	nativeRef    string
	local        bool
	interaction  *protocol.Interaction
	approvalReqs map[string]json.RawMessage
	// sending is the interaction id whose answer Respond is sending. The
	// resolution that answer causes can arrive before Respond returns, so
	// onServerRequestResolved leaves it to Respond, which knows whether the
	// send succeeded; resolvedWhileSending records that it arrived.
	sending              string
	resolvedWhileSending bool
	// queued holds approvals that arrived while another was pending. The
	// record shows one pending interaction, so they are shown one at a time,
	// in arrival order, once the current one resolves.
	queuedApprovals []*protocol.Interaction
	// createdAt is when the run was bound, for the unconfirmed-shadow
	// deadline: a retained-but-unconfirmed run shadows lookupHistory
	// in Lookup. After confirmTimeout, if still unconfirmed, the run stops
	// shadowing so lookupHistory can resolve the turn by clientId.
	createdAt time.Time
	// confirmed (the bool) is the durable ownership flag every consumer reads:
	// until the userMessage item carrying this run's clientUserMessageId is
	// observed, the run is TENTATIVE and must not be cancelled, attributed, or
	// reported admitted. confirmedCh closes once, to release a waiting Submit.
	confirmed   bool
	confirmedCh chan struct{}
	// cancelPending records a cancel that arrived while tentative, to be
	// delivered when the run confirms.
	cancelPending bool
	queued        bool
	// acked records that the endpoint acknowledged this run's terminal
	// result. The retained payload is released and Lookup reports
	// EvidenceNone for the key, so reconcile's ack replay converges instead
	// of re-asking every tick.
	acked bool
}

// Attachment is one running Codex thread reached through the shared
// app-server daemon. It implements core.Attachment.
type Attachment struct {
	// obsMu guards the activity observers (ObserveNotifications); it is
	// separate from mu so an observer never runs under the adapter lock.
	obsMu     sync.Mutex
	observers map[uint64]func(Notification)
	nextObs   uint64

	client   atomic.Pointer[Client] // set before Attach returns, read by read-loop-spawned paths
	threadID string
	targetID string
	epoch    string
	cwd      string
	approve  bool
	now      func() time.Time
	// confirmTimeout is how long Submit waits for our own userMessage item.
	// Overridable for tests.
	confirmTimeout time.Duration

	mu         sync.Mutex
	status     string
	activeTurn string
	// reviewer is the thread's approvals reviewer when it is not the user,
	// from thread/resume and then thread/settings/updated;
	// reviewerSeen keeps an update newer than the resume snapshot.
	reviewer     string
	reviewerSeen bool
	runs         map[requests.Key]*run
	byTurn       map[string]*run
	byClientID   map[string]*run // keyed by clientIDFor(key), not bare RequestID
	cancelIntent map[requests.Key]bool
	// terminalTurns is a bounded memo of turn IDs observed as terminal via
	// turn/completed, even when no byTurn entry exists (the confirming
	// userMessage item was missed in the race window). The RPC-response
	// guard reads this so a finished turn is never reinstalled as
	// active. Bounded via terminalTurnOrder (FIFO): when full, the OLDEST
	// observation is evicted — it is a race-window artefact, not a log.
	terminalTurns     map[string]bool
	terminalTurnOrder []string // FIFO of the same turn IDs; drives eviction
	// lostStateGen is bumped every time a notLoaded/systemError notification
	// tells us the app-server lost track of any previously-observed turn.
	// A turn/start RPC continuation captured the generation BEFORE its call;
	// if the generation advanced by the time the
	// continuation restores activeTurn/status, the restore would resurrect a
	// turn whose observing source is gone — the exact wedge the lost-state
	// handler just cleared. Stale continuations drop the restore instead.
	lostStateGen uint64
	listeners    map[int]func(core.NativeEvent)
	nextListener int
	offline      bool
	// ackedOrder is a FIFO of keys whose runs are terminal and acknowledged:
	// their retained payload is released and Lookup reports EvidenceNone, so
	// the runs/byClientID/byTurn entries exist only for stale correlation. To
	// bound memory, once the FIFO exceeds maxLiveRuns the
	// oldest acked run is fully dropped from all three maps. A later
	// Lookup/Cancel for it returns EvidenceNone/unknown exactly as a
	// compacted tombstone would; the durable store remains the source of
	// truth for the request's disposition.
	ackedOrder []requests.Key
	// ackedKeyOrder drives eviction of ackedKeys, the longer-lived memo that
	// lets a pruned run short-circuit Lookup. It outlives ackedOrder so a
	// compacted tombstone still converges.
	ackedKeyOrder []requests.Key
	// ackedKeys remembers which keys have been acknowledged, so a run pruned
	// from the bounded runs map (above) still short-circuits Lookup to
	// EvidenceNone instead of falling through to lookupHistory, whose copy
	// would look like fresh evidence and restart the ack loop. Bounded by the
	// same FIFO eviction as ackedOrder.
	ackedKeys map[requests.Key]bool
	// questionMu orders approval events: every path that changes the
	// pending approval holds it from the change through the emission, so
	// the endpoint sees EventQuestion and EventQuestionResolved in the order
	// the state changed. It is taken before mu, and never by a method the
	// endpoint calls while holding its own lock.
	questionMu sync.Mutex
	// resolvedEarly remembers request ids that serverRequest/resolved named
	// before the request worker delivered the request itself; such a
	// request is already answered and is never shown. FIFO-bounded.
	resolvedEarly      map[string]bool
	resolvedEarlyOrder []string
	// threadApprovals are the open decision approvals of turns no run of
	// ours owns, such as a turn the user started in the TUI, by threadKey.
	// At most maxThreadApprovals; past that the TUI answers alone.
	threadApprovals map[string]*threadApproval
}

// maxResolvedEarly bounds Attachment.resolvedEarly.
const maxResolvedEarly = 64

// maxThreadApprovals bounds Attachment.threadApprovals.
const maxThreadApprovals = 64

// threadApproval is one decision approval of a turn no run of ours owns.
// sending and resolvedWhileSending work as on run.
type threadApproval struct {
	turnID               string
	reqID                json.RawMessage
	inter                *protocol.Interaction
	sending              bool
	resolvedWhileSending bool
}

// threadKey names a thread approval by its turn and interaction id.
func threadKey(turnID, interactionID string) string { return turnID + "\x00" + interactionID }

// Option configures Attach.
type Option func(*Attachment)

// WithApprovals advertises approve_tool and answers command approvals from
// remote decisions. Off until fanout of approval requests to a second client
// is verified live.
func WithApprovals(on bool) Option { return func(a *Attachment) { a.approve = on } }

// WithTarget overrides the target id the endpoint addresses this attachment
// under (.13: the manifest target is the address). The native codex thread id
// is kept separately and is still used for the daemon protocol; only the
// advertised identity changes. Without it, the legacy codex:<thread> id is
// derived from the thread, preserving every existing --codex-socket user.
func WithTarget(targetID string) Option {
	return func(a *Attachment) {
		if targetID != "" {
			a.targetID = targetID
		}
	}
}

// Attach connects to the daemon socket, resumes threadID as a second client,
// and starts consuming its notifications.
func Attach(socketPath, threadID string, opts ...Option) (*Attachment, error) {
	// The attachment is built BEFORE the connection so its handlers can be
	// installed as Dial arguments: the read pump starts inside Dial, and
	// assigning handlers afterwards raced it (and dropped any frame that
	// arrived first).
	//
	// What makes that order safe is NOT that the handlers avoid a.client —
	// onNotification can reach it through onItem -> deliverPendingCancel.
	// It is that every path to a.client is reached only through runs,
	// byTurn, byClientID or listeners, and all four are empty until Submit
	// or Subscribe, neither of which can run before Attach returns. Keep
	// that true: a handler that touches a.client outside those maps would
	// read it before the assignment below.
	a := &Attachment{
		threadID:          threadID,
		targetID:          TargetID(threadID),
		epoch:             fmt.Sprintf("cx-%d", time.Now().UnixNano()),
		status:            "unknown",
		runs:              map[requests.Key]*run{},
		byTurn:            map[string]*run{},
		byClientID:        map[string]*run{},
		terminalTurns:     map[string]bool{},
		terminalTurnOrder: []string{},
		cancelIntent:      map[requests.Key]bool{},
		listeners:         map[int]func(core.NativeEvent){},
		ackedKeys:         map[requests.Key]bool{},
		ackedKeyOrder:     []requests.Key{},
		now:               time.Now,
		confirmTimeout:    confirmTimeout,
	}
	for _, o := range opts {
		o(a)
	}
	client, err := Dial(socketPath, Handlers{
		OnNotification:  a.onNotification,
		OnServerRequest: a.onServerRequest,
	})
	if err != nil {
		return nil, err
	}
	a.client.Store(client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	init := map[string]any{"clientInfo": map[string]string{"name": ClientName, "version": Version, "title": "AMQ Remote"}}
	if a.approve {
		// Codex 0.160 strips additionalPermissions from approval requests
		// sent to a client without the experimental API, and the approve
		// guard reads that field: without it a command that also asks for
		// more access would get a one-tap approve.
		init["capabilities"] = map[string]bool{"experimentalApi": true}
	}
	if err := client.Call(ctx, "initialize", init, nil); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var resumed struct {
		Thread struct {
			ID     string `json:"id"`
			Cwd    string `json:"cwd"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
		ApprovalsReviewer string `json:"approvalsReviewer"`
	}
	if err := client.Call(ctx, "thread/resume", map[string]any{"threadId": threadID}, &resumed); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("thread/resume %s: %w", threadID, err)
	}
	// The read pump has been live since Dial, so these fields are already
	// shared with onNotification: take the lock. And thread/resume answers
	// with a snapshot taken BEFORE any notification that arrived while the
	// call was in flight — applying its status unconditionally would undo a
	// turn/started we have already seen and publish "idle" for a running
	// thread. A turn we know about wins over the older snapshot.
	//
	// The client handle is published through an atomic.Pointer, not a plain
	// field — a read-loop-spawned
	// goroutine (deliverPendingCancel) can be scheduled in the window
	// between Dial returning and this store; the pointer publication is
	// synchronized, and the spawned path nil-checks, so no read observes a
	// partially constructed attachment. The maps-empty invariant below
	// (Submit/Subscribe cannot run before Attach returns) still holds and
	// remains the guard for every other a.client read: those are all
	// post-Attach, where the store is already visible.
	a.mu.Lock()
	a.cwd = resumed.Thread.Cwd
	if a.activeTurn == "" {
		a.status = threadStatus(resumed.Thread.Status.Type)
	}
	if !a.reviewerSeen {
		a.reviewer = autoReviewer(resumed.ApprovalsReviewer)
	}
	a.mu.Unlock()
	go func() {
		<-client.Done()
		a.mu.Lock()
		a.offline = true
		a.mu.Unlock()
		a.emit(core.NativeEvent{Type: core.EventStatus, Attachment: "offline"})
	}()
	return a, nil
}

// autoReviewer is Codex's approvalsReviewer when it is an automatic
// reviewer (auto_review, or the legacy guardian_subagent), which answers
// approvals itself so they never reach a client; "" for the user.
func autoReviewer(v string) string {
	if v == "user" {
		return ""
	}
	return v
}

// TargetID derives the stable target id for a thread.
// NativeSessionID is the attached thread's id, the identity relay sharing
// pins (core.NativeIdentifier).
func (a *Attachment) NativeSessionID() string { return a.threadID }

func TargetID(threadID string) string {
	// The full de-hyphenated thread id, never truncated: two distinct thread
	// ids must not collapse to one target. Codex thread ids are
	// UUIDv7 (32 hex), well within the 128-char target_id bound.
	return "codex:" + strings.ReplaceAll(threadID, "-", "")
}

func threadStatus(t string) string {
	switch t {
	case "idle":
		return "idle"
	case "active":
		return "busy"
	case "notLoaded", "systemError":
		return "unknown"
	}
	return "unknown"
}

// Close drops the connection. The thread keeps running in Codex.
func (a *Attachment) Close() error { return a.client.Load().Close() }

// Inspect implements core.Attachment.
func (a *Attachment) Inspect() protocol.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	att, status := "live", a.status
	if a.offline {
		att, status = "offline", "offline"
	}
	// Ranging over the runs map and keeping the last interaction would pick
	// whichever run the randomizer visited last, so the published
	// PendingInteraction would flap between Inspect calls with no state
	// change. The operator should answer the interaction that has been
	// waiting the longest: pick the OLDEST
	// interaction by run createdAt, ties broken by interaction id for full
	// determinism.
	var pending, active *string
	var pendingAt time.Time
	for _, r := range a.runs {
		if r.interaction == nil {
			continue
		}
		id := r.interaction.InteractionID
		if pending == nil || r.createdAt.Before(pendingAt) ||
			(r.createdAt.Equal(pendingAt) && id < *pending) {
			// The request that waits on it, so a surface can name the
			// approval it shows.
			ref := protocol.EncodeRef(r.key.CreatorHost, r.key.TargetID, r.key.RequestID)
			pending, active = &id, &ref
			pendingAt = r.createdAt
		}
	}
	return protocol.Session{
		Schema:             protocol.SchemaSession,
		TargetID:           a.targetID,
		Epoch:              a.epoch,
		Harness:            "codex",
		DisplayName:        "codex " + a.threadID,
		Project:            a.cwd,
		Attachment:         att,
		Status:             status,
		PendingInteraction: pending,
		ActiveRequestRef:   active,
		ApprovalReviewer:   a.reviewer,
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: true, CancelRequest: true, Steer: false,
			ApproveTool: a.approve, AnswerQuestion: false, Terminal: "unavailable",
		},
		Evidence:   &protocol.Evidence{Submit: "admitted", Completion: "run_terminal"},
		ObservedAt: protocol.FormatTime(time.Now()),
	}
}

// Submit implements core.Attachment. It never holds the attachment lock across
// a network call (that would stall the read loop that delivers the response),
// and for an idle turn/start it admits only after our own userMessage item
// confirms the text landed — the measured busy-join drops the caller's text
// and returns the running turn, which must not become an admitted phantom run.
func (a *Attachment) Submit(req core.BoundRequest) (core.Admission, error) {
	a.mu.Lock()
	if a.offline {
		a.mu.Unlock()
		return refusal(fmt.Errorf("%w: app-server connection is closed", ErrNotSent)), nil
	}
	if req.Epoch != a.epoch {
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeStaleEpoch}, nil
	}
	if a.cancelIntent[req.Key] {
		delete(a.cancelIntent, req.Key)
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeCancelledBeforeAdmission}, nil
	}
	if existing, ok := a.runs[req.Key]; ok {
		id := existing.runIDLocked()
		a.mu.Unlock()
		return core.Admission{Admitted: true, RunID: id}, nil
	}
	busy := a.status == "busy" || a.activeTurn != ""
	activeTurn := a.activeTurn
	a.mu.Unlock()
	// A turn sent while the seat's TUI still shows a startup screen would
	// end the hold that keeps the seat's own doorbell out of that screen.
	if seatStartupHeld(a.threadID) {
		return core.Admission{Code: protocol.CodeBusy, Message: "the Codex seat on this thread has not passed its startup screens yet"}, nil
	}

	input := []map[string]any{{"type": "text", "text": req.Input.Text}}
	// Core checked not_after before it dispatched, but its store write and
	// the wait for the writer slot come after that check. Every prompt send
	// below rechecks not_after at the writer, so a prompt never leaves late.
	var sendBy time.Time
	if exp, err := protocol.ParseTime(req.NotAfter); err == nil {
		sendBy = exp
	}

	switch {
	case req.Input.Deliver == protocol.DeliverSteer:
		// Steer interleaves into the running turn; expectedTurnId makes the
		// app-server reject if the active turn changed under us, so no
		// separate confirmation is needed.
		if !busy {
			return core.Admission{Code: protocol.CodeUnsupported, Message: "steer needs an active turn; use deliver=turn"}, nil
		}
		if err := a.call(sendBy, "turn/steer", map[string]any{"threadId": a.threadID, "expectedTurnId": activeTurn, "input": input, "clientUserMessageId": clientIDFor(req.Key)}, nil); err != nil {
			return refusal(err), nil
		}
		r := &run{key: req.Key, epoch: req.Epoch, state: protocol.StateRunning, turnID: activeTurn, confirmed: true, approvalReqs: map[string]json.RawMessage{}, createdAt: a.now()}
		a.mu.Lock()
		a.runs[req.Key] = r
		a.byClientID[clientIDFor(req.Key)] = r
		a.byTurn[activeTurn] = r
		a.mu.Unlock()
		return core.Admission{Admitted: true, RunID: a.runID(r)}, nil

	case busy && req.Input.Busy == protocol.BusyQueue:
		// Codex's own FIFO queue primitive; acceptance is Codex-native.
		if err := a.call(sendBy, "thread/queue/add", map[string]any{"threadId": a.threadID, "clientUserMessageId": clientIDFor(req.Key), "input": input}, nil); err != nil {
			return refusal(err), nil
		}
		r := &run{key: req.Key, epoch: req.Epoch, state: protocol.StateRunning, queued: true, approvalReqs: map[string]json.RawMessage{}, createdAt: a.now()}
		a.mu.Lock()
		a.runs[req.Key] = r
		a.byClientID[clientIDFor(req.Key)] = r
		a.mu.Unlock()
		return core.Admission{Admitted: true, RunID: a.runID(r)}, nil

	case busy:
		return core.Admission{Code: protocol.CodeBusy, Message: "a turn is active on this thread"}, nil
	}

	// Idle turn/start. Register the run first so onItem can confirm our
	// userMessage, then call unlocked, then require confirmation.
	r := &run{key: req.Key, epoch: req.Epoch, state: protocol.StateRunning, approvalReqs: map[string]json.RawMessage{}, confirmedCh: make(chan struct{}), createdAt: a.now()}
	a.mu.Lock()
	a.runs[req.Key] = r
	a.byClientID[clientIDFor(req.Key)] = r
	a.mu.Unlock()

	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	// Capture the lost-state generation before the RPC. A
	// notLoaded/systemError notification processed by the read pump while
	// this call is in flight means the app-server lost track of the turn we
	// are about to restore — the continuation must not resurrect it.
	a.mu.Lock()
	genBefore := a.lostStateGen
	a.mu.Unlock()
	if err := a.call(sendBy, "turn/start", map[string]any{"threadId": a.threadID, "input": input, "clientUserMessageId": clientIDFor(req.Key)}, &res); err != nil {
		// Split pre-send vs post-send. ErrNotSent (marshal error, write
		// error, connection closed before
		// write) and rpcError (server responded with an error) are both
		// unambiguous: the prompt either never left or was positively
		// refused. dropRun + refuse (retry is safe). Post-send failures
		// (closed before reply, ctx deadline, unmarshal error) are ambiguous:
		// the turn may be running. Keep the correlation, return a non-nil
		// error so the endpoint records uncertain.
		var rpc *rpcError
		if errors.Is(err, ErrNotSent) || errors.As(err, &rpc) {
			a.dropRun(req.Key)
			return refusal(err), nil
		}
		// Post-send ambiguity: keep the correlation so Lookup and
		// history can still resolve it. Return a non-nil error (not a refusal
		// code) so the endpoint records uncertain.
		return core.Admission{RunID: a.runID(r)}, err
	}
	if res.Turn.ID == "" {
		// A successful RPC response with no turn id is ambiguous, not a
		// definitive refusal. The server may have accepted the prompt under a
		// turn we have not observed yet. Preserve the correlation and return
		// a non-nil error so the endpoint records uncertain.
		return core.Admission{RunID: a.runID(r)}, fmt.Errorf("turn/start returned no turn id")
	}
	a.mu.Lock()
	// Before this goroutine resumes, the read pump may have finished the turn
	// (confirming notification, result, idle status), or processed a
	// notLoaded/systemError that invalidated the state this continuation
	// would restore. Restoring activeTurn in either case would wedge the
	// adapter busy with no later event to clear it. A finished run of ours is
	// a POSITIVE fact that restores nothing, so it is checked FIRST and
	// reports admitted: a lost-state notification cannot downgrade a known
	// outcome to uncertain. Only our own evidence makes the run ours: a
	// terminal state (reachable only through byTurn, which only confirmation
	// binds) or a memo hit on a run our userMessage item or history confirmed.
	if r.state.Terminal() || (r.confirmed && a.terminalTurns[res.Turn.ID]) {
		rid := r.runIDLocked()
		a.mu.Unlock()
		return core.Admission{Admitted: true, RunID: rid}, nil
	}
	// The returned turn already finished without our userMessage item. The
	// busy-join returns a running foreign turn and drops our text, so this
	// proves nothing about our request. Restore nothing (the turn is over),
	// keep the run correlation so history can still resolve it, and report
	// uncertain.
	if a.terminalTurns[res.Turn.ID] {
		rid := r.runIDLocked()
		a.mu.Unlock()
		return core.Admission{RunID: rid}, fmt.Errorf("turn %s finished before our own userMessage item confirmed the text; the request may not have run", res.Turn.ID)
	}
	// An advanced lostStateGen drops the restore, keeps the run correlation
	// (the text may still be running, so Lookup and history can resolve it)
	// and reports uncertain, like the post-send ambiguity path.
	if genBefore != a.lostStateGen {
		rid := r.runIDLocked()
		a.mu.Unlock()
		return core.Admission{RunID: rid}, fmt.Errorf("thread state was lost (notLoaded/systemError) while turn/start was in flight; not restoring activeTurn for turn %s", res.Turn.ID)
	}
	if r.turnID == "" {
		r.turnID = res.Turn.ID
		// byTurn is NOT bound here: the returned turn id may belong to a turn
		// started in the race window that dropped our text. It is bound only
		// when confirmRun sees our own userMessage item.
	}
	a.activeTurn = res.Turn.ID
	a.status = "busy"
	confirmedCh := r.confirmedCh
	a.mu.Unlock()

	select {
	case <-confirmedCh:
		return core.Admission{Admitted: true, RunID: a.runID(r)}, nil
	case <-time.After(a.confirmTimeout):
		// turn/start already returned a turn id: the text may be RUNNING in
		// Codex. Absence of our confirmation is NOT proof of refusal, and a
		// refusal code would commit a terminal `rejected` record that
		// reconcile never revisits — the caller retries with a fresh id and
		// the prompt runs twice. Report an ERROR so the endpoint records
		// UNCERTAIN, and KEEP the correlation (no dropRun) so Lookup and
		// lookupHistory can still resolve it. runIDLocked needs a.mu:
		// confirmRun writes r.turnID from the read-loop goroutine, so every
		// runID read goes through a.runID or runIDLocked under a.mu.
		a.mu.Lock()
		rid := r.runIDLocked()
		a.mu.Unlock()
		return core.Admission{RunID: rid}, fmt.Errorf("turn/start was not confirmed by our own userMessage item within %s; the turn may be running", a.confirmTimeout)
	case <-a.client.Load().Done():
		a.mu.Lock()
		rid := r.runIDLocked()
		a.mu.Unlock()
		return core.Admission{RunID: rid}, errors.New("app-server closed during turn/start; the turn may be running")
	}
}

// call runs one app-server request with a bounded timeout and no lock held.
// A request still unwritten at a non-zero sendBy is not sent.
func (a *Attachment) call(sendBy time.Time, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return a.client.Load().callBefore(ctx, sendBy, method, params, result)
}

// memoTerminal records turnID as an observed-terminal turn and keeps the
// FIFO bound. Caller holds a.mu.
func (a *Attachment) memoTerminal(turnID string) {
	if turnID == "" || a.terminalTurns[turnID] {
		return
	}
	a.terminalTurns[turnID] = true
	a.terminalTurnOrder = append(a.terminalTurnOrder, turnID)
	// Bound: evict the OLDEST observed entry when the memo grows beyond a
	// race-window artefact size. FIFO via terminalTurnOrder: Go map
	// iteration order is randomized, so walking the map to "evict the
	// oldest" evicts an arbitrary entry — possibly the turn whose RPC
	// response is still in flight, reinstating the wedge this memo closes.
	if len(a.terminalTurnOrder) > maxLiveRuns {
		oldest := a.terminalTurnOrder[0]
		a.terminalTurnOrder = a.terminalTurnOrder[1:]
		delete(a.terminalTurns, oldest)
	}
}

// dropRun removes a run binding when admission failed, so a retry is clean.
func (a *Attachment) dropRun(key requests.Key) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.runs[key]; ok {
		delete(a.byClientID, clientIDFor(key))
		if r.turnID != "" {
			delete(a.byTurn, r.turnID)
		}
		delete(a.runs, key)
	}
}

// confirmRun marks a run's native ownership established, binds byTurn, and
// releases a waiting Submit. The caller holds a.mu. It returns whether a
// cancel was pending so the caller can deliver it outside the lock.
func (a *Attachment) confirmRun(r *run, turnID string) (cancelPending bool) {
	if r.confirmed {
		return false
	}
	r.confirmed = true
	if turnID != "" {
		r.turnID = turnID
		a.byTurn[turnID] = r
	}
	if r.confirmedCh != nil {
		select {
		case <-r.confirmedCh:
		default:
			close(r.confirmedCh)
		}
	}
	return r.cancelPending
}

// deliverPendingCancel sends the interrupt for a cancel that arrived while the
// run was tentative, now that ownership is confirmed. No lock held.
//
// It is an early interrupt, not the owner of the cancel. CancelExact answered
// that cancel with cancel_requested, which the endpoint keeps on the record;
// admission of a cancel_requested record calls CancelExact again, and while
// the record owes the cancel, reconcile re-drives CancelExact each tick. Once
// the run is confirmed those calls send turn/interrupt themselves, so a
// failed send here, or a nil client, changes no state and loses no intent.
//
// This runs on a goroutine spawned from the read loop, so it can be scheduled
// before Attach finishes publishing a.client (the store happens after Dial
// returns, while the pump is already live). The nil check makes that ordering
// harmless instead of racing the store.
func (a *Attachment) deliverPendingCancel(turnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := a.client.Load()
	if client == nil {
		return
	}
	// The resulting turn/completed(interrupted) drives the cancelled state.
	_ = client.Call(ctx, "turn/interrupt", map[string]any{"threadId": a.threadID, "turnId": turnID}, nil)
}

// historyEvidenceLocked is the evidence for a run that thread history proved
// ours. The caller holds a.mu for the whole snapshot: the read pump writes
// the run's text and state under a.mu, so no field is read after unlock and
// the evidence never mixes two moments. A terminal run is history-proven
// final: a restarted attachment retains nothing in memory, so this class
// tells the endpoint the ack replay may converge on it.
func (r *run) historyEvidenceLocked() core.Evidence {
	ev := core.Evidence{Known: true, Admitted: true, RunID: r.runIDLocked(), State: r.state, Result: r.result()}
	if r.state.Terminal() {
		ev.Class = core.EvidenceHistoryTerminated
	}
	return ev
}

// runIDLocked returns the run's correlation id. The caller MUST hold a.mu:
// r.turnID is written by confirmRun from the read-loop goroutine. Every
// external caller goes through a.runID, which takes the lock.
func (r *run) runIDLocked() string {
	if r.turnID != "" {
		return "turn:" + r.turnID
	}
	return "queued:" + r.key.RequestID
}

// runID is the lock-taking accessor for external callers. It is the ONLY
// way to read a run's runID outside a.mu. Every read of a run's
// mutable fields (turnID, confirmed, state, text, createdAt) goes through a
// small accessor that takes a.mu; no caller touches r.<field> directly.
func (a *Attachment) runID(r *run) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return r.runIDLocked()
}

// clientIDFor returns the native correlation token for a request key.
// The endpoint's identity is (CreatorHost, TargetID, RequestID), so two
// creator hosts can legitimately share a UUID. Indexing by bare RequestID
// would attribute one caller's failed run to another's live request. The
// composite token preserves the full namespace. This token is sent as
// clientUserMessageId to the server and echoed back in history items; the
// byClientID map is keyed by it.
func clientIDFor(key requests.Key) string {
	return protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
}

func refusal(err error) core.Admission {
	if errors.Is(err, errSendDeadline) {
		return core.Admission{Code: protocol.CodeExpired, Message: "not_after passed before the prompt was sent"}
	}
	var rpc *rpcError
	if errors.As(err, &rpc) {
		return core.Admission{Code: protocol.CodeNativeError, Message: rpc.Message}
	}
	return core.Admission{Code: protocol.CodeNativeError, Message: err.Error()}
}

// Lookup implements core.Attachment. It answers from retained runs first and
// then from the thread's own history, where the user message carries our
// request id as clientId.
func (a *Attachment) Lookup(key requests.Key, epoch string) (core.Evidence, error) {
	a.mu.Lock()
	if r, ok := a.runs[key]; ok && r.epoch == epoch {
		if r.acked {
			// Released: the endpoint acknowledged this result and holds it
			// durably. Report that nothing is retained — and do NOT fall
			// through to the thread/read history, whose copy would look like
			// fresh evidence and restart the ack loop.
			ev := core.Evidence{Known: true, Admitted: true, Class: core.EvidenceNone, RunID: r.runIDLocked(), State: r.state}
			a.mu.Unlock()
			return ev, nil
		}
		ev := core.Evidence{Known: true, RunID: r.runIDLocked(), State: r.state, LocalIntervention: r.local, Interaction: r.interaction}
		switch {
		case r.confirmed || r.queued:
			// A confirmed running turn, or a Codex-native-accepted queue item:
			// real admission.
			ev.Class = core.EvidenceConfirmed
			ev.Admitted = true
			if r.state.Terminal() {
				ev.Result = r.result()
			}
		default:
			// Bound but native ownership not yet proven: tentative, never
			// admitted, so reconcile leaves it running and never rejects it.
			//
			// A retained-but-unconfirmed run that shadowed lookupHistory
			// forever would leave the record at Uncertain and never deliver a
			// real completed result. After confirmTimeout,
			// if still unconfirmed, STOP shadowing — fall through to
			// lookupHistory, which resolves the turn by clientId == key.RequestID
			// and can deliver the completed result. The deadline is the same
			// bound Submit already waited: if our userMessage item never
			// arrived within confirmTimeout, the race window dropped our text
			// and the run will never confirm.
			if a.now().Sub(r.createdAt) >= a.confirmTimeout {
				a.mu.Unlock()
				return a.lookupHistory(key, epoch)
			}
			ev.Class = core.EvidenceTentative
		}
		a.mu.Unlock()
		return ev, nil
	}
	// A run pruned from the bounded runs map after ack still
	// short-circuits to EvidenceNone here, so it never falls through to
	// lookupHistory (whose copy would look like fresh evidence and restart
	// the ack loop). The durable store is the source of truth for the
	// disposition.
	if a.ackedKeys[key] {
		a.mu.Unlock()
		return core.Evidence{Known: true, Admitted: true, Class: core.EvidenceNone, State: protocol.StateCompleted}, nil
	}
	a.mu.Unlock()
	return a.lookupHistory(key, epoch)
}

func (a *Attachment) lookupHistory(key requests.Key, epoch string) (core.Evidence, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var res struct {
		Thread struct {
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
				Items []struct {
					Type     string `json:"type"`
					ClientID string `json:"clientId"`
					Text     string `json:"text"`
				} `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := a.client.Load().Call(ctx, "thread/read", map[string]any{"threadId": a.threadID, "includeTurns": true}, &res); err != nil {
		return core.Evidence{}, err
	}
	for _, t := range res.Thread.Turns {
		mine := false
		var text strings.Builder
		for _, it := range t.Items {
			if it.Type == "userMessage" && it.ClientID == clientIDFor(key) {
				mine = true
			}
			if it.Type == "agentMessage" {
				text.Reset()
				text.WriteString(it.Text)
			}
		}
		if !mine {
			continue
		}
		var state protocol.State
		var errText string
		switch t.Status {
		case "completed":
			state = protocol.StateCompleted
		case "failed":
			state = protocol.StateFailed
			if t.Error != nil {
				errText = t.Error.Message
			}
		case "interrupted":
			state = protocol.StateCancelled
		default:
			state = protocol.StateRunning
		}
		// When history resolves a run as terminal, MAKE THE IN-MEMORY RUN
		// TERMINAL. Record the turn id so the correlation exists (byTurn), set
		// the terminal state, and populate the result text so AcknowledgeResult
		// can match the digest and release it. A run we have proven finished
		// must not stay "running" in our own map forever, re-reading the whole
		// transcript every tick.
		//
		// r.result() is the ONLY place a protocol.Result is constructed in
		// this package — lookupHistory does not build one directly, so the
		// bounding point (MaxResultBytes, truncated) applies here too.
		nativeRef := "codex thread " + a.threadID + " turn " + t.ID
		if state.Terminal() {
			a.mu.Lock()
			// When history establishes a turn terminal, clear a.activeTurn
			// ONLY IF it is that same turn
			// id. If a newer turn is active, leave it alone — clearing a newer
			// turn is a worse bug.
			if a.activeTurn == t.ID {
				a.activeTurn = ""
				a.status = "idle"
			}
			// Record through memoTerminal like every other memo writer. A
			// bare map write would bypass the FIFO: the entry would never
			// enter terminalTurnOrder, so eviction (which measures the SLICE)
			// would never remove it and the map would grow without bound, one
			// entry per distinct history-recovered turn.
			a.memoTerminal(t.ID)
			if r, ok := a.runs[key]; ok {
				r.turnID = t.ID
				a.byTurn[t.ID] = r
				r.state = state
				r.errText = errText
				r.nativeRef = nativeRef
				r.text.Reset()
				r.text.WriteString(text.String())
				r.confirmed = true
				ev := r.historyEvidenceLocked()
				a.mu.Unlock()
				return ev, nil
			}
			a.mu.Unlock()
		}
		// No in-memory run (e.g. after restart) or non-terminal: build the
		// result through the same helper so the bounding point is one.
		// INSTALL a confirmed run entry so CancelExact can act on it.
		// Without this, a surviving run after restart cannot be cancelled —
		// the adapter has no entry, so CancelExact records intent only.
		// Install for BOTH terminal and live results: a terminal run still
		// needs the entry so AcknowledgeResult can release it.
		r := &run{
			key: key, turnID: t.ID, state: state,
			epoch:   epoch,
			errText: errText, nativeRef: nativeRef,
			confirmed: true, createdAt: a.now(),
			approvalReqs: map[string]json.RawMessage{},
		}
		r.text.WriteString(text.String())
		a.mu.Lock()
		if existing, ok := a.runs[key]; ok {
			// Evidence only ADVANCES: fill only what is empty, never
			// overwrite state/text the pump set.
			if existing.turnID == "" {
				existing.turnID = t.ID
			}
			// Every install path sets an epoch; an empty one is filled
			// defensively from the requested epoch: history has proven this
			// is the same native turn, so the entry answers for the epoch the
			// endpoint holds.
			if existing.epoch == "" {
				existing.epoch = epoch
			}
			existing.confirmed = true
			// If the pump already moved to terminal, keep its state/text.
			if !existing.state.Terminal() {
				existing.state = state
				existing.errText = errText
				existing.nativeRef = nativeRef
				existing.text.Reset()
				existing.text.WriteString(text.String())
			}
			r = existing
		} else {
			a.runs[key] = r
			a.byClientID[clientIDFor(key)] = r
			// If this is a terminal turn recovered from history after
			// restart, clear a.activeTurn if it matches (the pump is dead).
			if state.Terminal() && a.activeTurn == t.ID {
				a.activeTurn = ""
				a.status = "idle"
			}
		}
		if t.ID != "" && r.turnID != "" {
			a.byTurn[r.turnID] = r
		}
		ev := r.historyEvidenceLocked()
		a.mu.Unlock()
		return ev, nil
	}
	// No turn carried our clientId. That is NOT positive proof the request was
	// never admitted: the clientId echo is schema-backed but unverified
	// against a live app-server, and a single omitted field would otherwise
	// make the first reconcile after a restart REJECT every in-flight request
	// while its turns keep running. Absence of correlation is unknown —
	// uncertain, keep correlation — never EvidenceNone.
	return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
}

// CancelExact implements core.Attachment.
func (a *Attachment) CancelExact(key requests.Key, epoch string) (core.CancelEvidence, error) {
	a.mu.Lock()
	r, ok := a.runs[key]
	if !ok {
		a.cancelIntent[key] = true
		a.mu.Unlock()
		return core.CancelEvidence{Disposition: protocol.CancelRequested, Message: "intent recorded before admission"}, nil
	}
	if r.epoch != epoch || r.state.Terminal() {
		a.mu.Unlock()
		return core.CancelEvidence{Disposition: protocol.CancelNoopTerminal}, nil
	}
	if !r.confirmed && !r.queued {
		// We do not yet own a turn; interrupting r.turnID could hit a foreign
		// turn. Record intent; confirmRun delivers it once ownership is proven.
		r.cancelPending = true
		a.mu.Unlock()
		return core.CancelEvidence{Disposition: protocol.CancelRequested, Message: "intent recorded before native ownership"}, nil
	}
	turnID, queued := r.turnID, r.queued
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if queued && turnID == "" {
		if err := a.client.Load().Call(ctx, "thread/queue/delete", map[string]any{"threadId": a.threadID, "clientUserMessageId": clientIDFor(key)}, nil); err != nil {
			return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: err.Error()}, nil
		}
		a.mu.Lock()
		r.state = protocol.StateCancelled
		a.mu.Unlock()
		a.emit(core.NativeEvent{Type: core.EventRunCancelled, Key: key, RunID: a.runID(r)})
		return core.CancelEvidence{Disposition: protocol.CancelConfirmed}, nil
	}
	if err := a.client.Load().Call(ctx, "turn/interrupt", map[string]any{"threadId": a.threadID, "turnId": turnID}, nil); err != nil {
		return core.CancelEvidence{Disposition: protocol.CancelRequested, Message: err.Error()}, nil
	}
	// Confirmation arrives as turn/completed with status interrupted.
	return core.CancelEvidence{Disposition: protocol.CancelRequested, Message: "interrupt sent for " + turnID}, nil
}

// Respond implements core.Attachment for command approvals.
func (a *Attachment) Respond(key requests.Key, epoch, interactionID, option string) (protocol.Code, error) {
	a.mu.Lock()
	r, ok := a.runs[key]
	if !ok || r.epoch != epoch || r.interaction == nil || r.interaction.InteractionID != interactionID {
		a.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	}
	reqID, ok := r.approvalReqs[interactionID]
	if !ok {
		a.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	}
	allowed := false
	for _, o := range r.interaction.Options {
		if o == option {
			allowed = true
		}
	}
	if !allowed {
		a.mu.Unlock()
		return protocol.CodeInvalid, nil
	}
	if r.sending != "" {
		a.mu.Unlock()
		return "", errAnswerInFlight
	}
	r.sending, r.resolvedWhileSending = interactionID, false
	a.mu.Unlock()
	err := a.client.Load().Respond(reqID, map[string]string{"decision": option})
	// The send is done; from here the pending approval changes, so hold the
	// question order until the events are out.
	a.questionMu.Lock()
	defer a.questionMu.Unlock()
	if err != nil {
		a.mu.Lock()
		resolved := r.resolvedWhileSending
		r.sending, r.resolvedWhileSending = "", false
		var next *protocol.Interaction
		if resolved {
			// Another client answered while our send failed: resolved
			// elsewhere, and our answer never reached the harness.
			delete(r.approvalReqs, interactionID)
			r.interaction = nil
			next = r.advanceLocked()
		}
		a.mu.Unlock()
		if resolved {
			a.emitResolved(r, false, next)
			return protocol.CodeAlreadyResolved, nil
		}
		return "", err
	}
	// Tear down the approval state ONLY after Respond succeeds. Tearing it
	// down before the send would let a transport failure leave
	// r.interaction == nil, and a retry would return CodeAlreadyResolved
	// while the native question is still open.
	a.mu.Lock()
	_, open := r.approvalReqs[interactionID]
	open = open || r.resolvedWhileSending
	delete(r.approvalReqs, interactionID)
	r.interaction, r.sending, r.resolvedWhileSending = nil, "", false
	var next *protocol.Interaction
	if open {
		next = r.advanceLocked()
	}
	a.mu.Unlock()
	if open {
		a.emitResolved(r, true, next)
	}
	return "", nil
}

// errAnswerInFlight refuses a second send of the same answer while the
// first is in flight; the endpoint keeps that answer's intent. A different
// answer never gets here: the endpoint refuses it first.
var errAnswerInFlight = errors.New("an answer to this approval is already being sent")

// RespondThread implements core.ThreadResponder: it answers a decision
// approval of a turn no run of ours owns, as Respond answers one of ours.
func (a *Attachment) RespondThread(turnID, interactionID, option string) (protocol.Code, error) {
	k := threadKey(turnID, interactionID)
	a.mu.Lock()
	ta := a.threadApprovals[k]
	switch {
	case ta == nil:
		a.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	case offered(ta.inter.Options, option) == "":
		a.mu.Unlock()
		return protocol.CodeInvalid, nil
	case ta.sending:
		a.mu.Unlock()
		return "", errAnswerInFlight
	}
	ta.sending, ta.resolvedWhileSending = true, false
	a.mu.Unlock()
	err := a.client.Load().Respond(ta.reqID, map[string]string{"decision": option})
	a.questionMu.Lock()
	defer a.questionMu.Unlock()
	a.mu.Lock()
	resolved, open := ta.resolvedWhileSending, a.threadApprovals[k] == ta
	ta.sending, ta.resolvedWhileSending = false, false
	if open && (err == nil || resolved) {
		delete(a.threadApprovals, k)
	}
	a.mu.Unlock()
	switch {
	case err != nil && !resolved:
		return "", err
	case err != nil:
		// Another client answered while our send failed.
		a.emitThreadResolved(ta, false, "")
		return protocol.CodeAlreadyResolved, nil
	case open:
		a.emitThreadResolved(ta, true, "")
	}
	return "", nil
}

// emitThreadResolved reports a thread approval resolved: by our answer
// (remote), by the harness, or with outcome when it is set.
func (a *Attachment) emitThreadResolved(ta *threadApproval, remote bool, outcome protocol.ResolutionOutcome) {
	a.emit(core.NativeEvent{Type: core.EventThreadQuestionResolved, Turn: ta.turnID,
		Interaction: &protocol.Interaction{InteractionID: ta.inter.InteractionID}, Remote: remote, Outcome: outcome})
}

// endThreadApprovals resolves every thread approval of a turn that ended
// while it was open.
func (a *Attachment) endThreadApprovals(turnID string) {
	a.questionMu.Lock()
	defer a.questionMu.Unlock()
	a.mu.Lock()
	var ended []*threadApproval
	for k, ta := range a.threadApprovals {
		if ta.turnID == turnID && !ta.sending {
			ended = append(ended, ta)
			delete(a.threadApprovals, k)
		}
	}
	a.mu.Unlock()
	for _, ta := range ended {
		a.emitThreadResolved(ta, false, protocol.ResolutionRunEnded)
	}
}

// advanceLocked makes the oldest queued approval that Codex still holds the
// pending one, and returns it, or nil. The caller holds a.mu and has
// cleared r.interaction.
func (r *run) advanceLocked() *protocol.Interaction {
	for len(r.queuedApprovals) > 0 {
		next := r.queuedApprovals[0]
		r.queuedApprovals = r.queuedApprovals[1:]
		if _, open := r.approvalReqs[next.InteractionID]; open {
			r.interaction = next
			return next
		}
	}
	return nil
}

// emitResolved reports the pending approval resolved, then the next queued
// approval, if any, as the new pending one.
func (a *Attachment) emitResolved(r *run, remote bool, next *protocol.Interaction) {
	key, runID := r.key, a.runID(r)
	a.emit(core.NativeEvent{Type: core.EventQuestionResolved, Key: key, RunID: runID, Remote: remote})
	if next != nil {
		a.emit(core.NativeEvent{Type: core.EventQuestion, Key: key, RunID: runID, Interaction: next})
	}
}

// onServerRequestResolved clears an approval that another client, such as
// the Codex terminal, answered first. The app-server drops a later answer
// without an error, so without this a late remote answer would report
// success for a decision it did not make.
func (a *Attachment) onServerRequestResolved(n Notification) {
	a.questionMu.Lock()
	defer a.questionMu.Unlock()
	var p struct {
		ThreadID  string          `json:"threadId"`
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != a.threadID || len(p.RequestID) == 0 {
		return
	}
	a.mu.Lock()
	for _, r := range a.runs {
		for id, reqID := range r.approvalReqs {
			if !sameRequestID(reqID, p.RequestID) {
				continue
			}
			if r.sending == id {
				// Our answer is in flight: Respond reports the resolution
				// once it knows whether the send succeeded.
				r.resolvedWhileSending = true
				a.mu.Unlock()
				return
			}
			delete(r.approvalReqs, id)
			if r.interaction == nil || r.interaction.InteractionID != id {
				// A queued approval, never shown: nothing to report.
				a.mu.Unlock()
				return
			}
			r.interaction = nil
			next := r.advanceLocked()
			a.mu.Unlock()
			a.emitResolved(r, false, next)
			return
		}
	}
	for k, ta := range a.threadApprovals {
		if !sameRequestID(ta.reqID, p.RequestID) {
			continue
		}
		if ta.sending {
			ta.resolvedWhileSending = true
			a.mu.Unlock()
			return
		}
		delete(a.threadApprovals, k)
		a.mu.Unlock()
		a.emitThreadResolved(ta, false, "")
		return
	}
	// Server requests reach onServerRequest through a worker queue, so the
	// resolution can overtake its own request. Remember it.
	if key := compactID(p.RequestID); key != "" && !a.resolvedEarly[key] {
		if a.resolvedEarly == nil {
			a.resolvedEarly = map[string]bool{}
		}
		a.resolvedEarly[key] = true
		a.resolvedEarlyOrder = append(a.resolvedEarlyOrder, key)
		if len(a.resolvedEarlyOrder) > maxResolvedEarly {
			delete(a.resolvedEarly, a.resolvedEarlyOrder[0])
			a.resolvedEarlyOrder = a.resolvedEarlyOrder[1:]
		}
	}
	a.mu.Unlock()
}

// approvalPrompt is the owner-facing text of one approval request: what
// Codex asks to do, where, and why.
func approvalPrompt(method string, command any, cwd, reason string) string {
	var b strings.Builder
	switch c := command.(type) {
	case string:
		b.WriteString(c)
	case nil:
		if method == methodFileChangeApproval {
			b.WriteString("apply a file change")
		}
	default:
		if raw, err := json.Marshal(c); err == nil {
			b.Write(raw)
		}
	}
	if cwd != "" {
		fmt.Fprintf(&b, "\nin %s", cwd)
	}
	if reason != "" {
		fmt.Fprintf(&b, "\nreason: %s", reason)
	}
	return b.String()
}

// isAbsent reports whether an optional JSON field is missing or null.
func isAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// offered returns option when options contains it, and "" otherwise.
func offered(options []string, option string) string {
	for _, o := range options {
		if o == option {
			return option
		}
	}
	return ""
}

// sameRequestID compares two JSON-RPC ids by their compact encoding.
func sameRequestID(x, y json.RawMessage) bool {
	cx := compactID(x)
	return cx != "" && cx == compactID(y)
}

// requestPart is a JSON-RPC id in the opaque grammar of an interaction id:
// a string id as it is, a number in its compact form.
func requestPart(id json.RawMessage) string {
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return compactID(id)
}

// compactID is the compact JSON encoding of a JSON-RPC id, or "".
func compactID(id json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, id) != nil {
		return ""
	}
	return b.String()
}

// AcknowledgeResult implements core.Attachment. Codex keeps the transcript;
// nothing is retained here beyond the process.
// AcknowledgeResult implements core.Attachment: it releases the retained
// terminal evidence for the key. It was a no-op, so the convergence
// precondition the endpoint relies on ("once a native ack lands the
// attachment retains nothing and Lookup reports EvidenceNone") never held for
// codex: every terminal record re-asked Lookup on every reconcile tick,
// forever. The digest must name exactly this run's retained result, so a
// stale or foreign ack can never release a different request's evidence.
func (a *Attachment) AcknowledgeResult(key requests.Key, epoch, digest string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.runs[key]
	if !ok || r.epoch != epoch || !r.state.Terminal() || r.acked {
		return
	}
	if digest == "" || digest != protocol.EvidenceDigest(r.result()) {
		return
	}
	// Release the payload; the endpoint holds it durably now.
	r.acked = true
	r.text.Reset()
	r.errText = ""
	// Bound the in-memory correlation maps. A terminal+acked
	// run is fully resolved; keep it only as long as the FIFO cap allows, then
	// drop it from runs/byClientID/byTurn. The durable store retains the
	// disposition; a later Lookup returns EvidenceNone (the run is acked) and
	// a later Cancel returns unknown, matching a compacted tombstone.
	//
	// ackedKeys is a longer-lived memo than the run structs: it lets a pruned
	// run short-circuit Lookup to EvidenceNone without hitting lookupHistory.
	// It is bounded separately (ackedKeyOrder) so it cannot grow without
	// limit, but outlives the run struct long enough for a reconcile tick to
	// converge on a compacted tombstone.
	a.ackedKeys[key] = true
	a.ackedKeyOrder = append(a.ackedKeyOrder, key)
	for len(a.ackedKeyOrder) > ackedKeyMemoCap {
		oldestKey := a.ackedKeyOrder[0]
		a.ackedKeyOrder = a.ackedKeyOrder[1:]
		delete(a.ackedKeys, oldestKey)
	}
	a.ackedOrder = append(a.ackedOrder, key)
	for len(a.ackedOrder) > maxLiveRuns {
		oldest := a.ackedOrder[0]
		a.ackedOrder = a.ackedOrder[1:]
		if old, ok := a.runs[oldest]; ok {
			delete(a.byClientID, clientIDFor(oldest))
			if old.turnID != "" {
				delete(a.byTurn, old.turnID)
			}
			delete(a.runs, oldest)
		}
	}
}

// Subscribe implements core.Attachment.
func (a *Attachment) Subscribe(fn func(core.NativeEvent)) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextListener++
	id := a.nextListener
	a.listeners[id] = fn
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.listeners, id)
	}
}

func (a *Attachment) emit(ev core.NativeEvent) {
	a.mu.Lock()
	fns := make([]func(core.NativeEvent), 0, len(a.listeners))
	for _, fn := range a.listeners {
		fns = append(fns, fn)
	}
	a.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

// ObserveNotifications adds fn as a read-only observer of the app-server
// notifications this attachment receives, called on the read pump after
// the adapter's own handling. It is the activity export seam: fn
// must not block (activity.Sink.Enqueue does not) and gains no control. The
// returned func removes the observer.
func (a *Attachment) ObserveNotifications(fn func(Notification)) (stop func()) {
	a.obsMu.Lock()
	defer a.obsMu.Unlock()
	if a.observers == nil {
		a.observers = map[uint64]func(Notification){}
	}
	a.nextObs++
	id := a.nextObs
	a.observers[id] = fn
	return func() {
		a.obsMu.Lock()
		delete(a.observers, id)
		a.obsMu.Unlock()
	}
}

func (a *Attachment) notifyObservers(n Notification) {
	a.obsMu.Lock()
	fns := make([]func(Notification), 0, len(a.observers))
	for _, fn := range a.observers {
		fns = append(fns, fn)
	}
	a.obsMu.Unlock()
	for _, fn := range fns {
		fn(n)
	}
}

func (a *Attachment) onNotification(n Notification) {
	defer a.notifyObservers(n)
	switch n.Method {
	case "turn/started":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != a.threadID {
			return
		}
		a.mu.Lock()
		a.activeTurn = p.Turn.ID
		a.status = "busy"
		a.mu.Unlock()
	case "thread/settings/updated":
		var p struct {
			ThreadID       string `json:"threadId"`
			ThreadSettings struct {
				ApprovalsReviewer string `json:"approvalsReviewer"`
			} `json:"threadSettings"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != a.threadID {
			return
		}
		a.mu.Lock()
		a.reviewer, a.reviewerSeen = autoReviewer(p.ThreadSettings.ApprovalsReviewer), true
		a.mu.Unlock()
	case "item/started", "item/completed":
		a.onItem(n)
	case methodServerRequestResolved:
		a.onServerRequestResolved(n)
	case "turn/completed":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != a.threadID {
			return
		}
		a.endThreadApprovals(p.Turn.ID)
		a.mu.Lock()
		if a.activeTurn == p.Turn.ID {
			a.activeTurn = ""
			a.status = "idle"
		}
		// Record the terminal observation even when no byTurn entry exists
		// (the confirming userMessage item was missed), so the RPC-response
		// guard never reinstalls a finished turn as active. Record it
		// UNCONDITIONALLY: every arm of the status switch below (including
		// default -> StateFailed) is terminal, and a narrower status list
		// here would re-derive terminality and wedge for any status it
		// misses. The bound is owned by memoTerminal (FIFO via
		// terminalTurnOrder: Go map iteration order is random, so evicting by
		// walking the map could drop the turn whose RPC response is still in
		// flight).
		a.memoTerminal(p.Turn.ID)
		r, ok := a.byTurn[p.Turn.ID]
		if !ok || r.state.Terminal() {
			a.mu.Unlock()
			return
		}
		var ev core.NativeEvent
		switch p.Turn.Status {
		case "completed":
			r.state = protocol.StateCompleted
			r.nativeRef = "codex thread " + a.threadID + " turn " + p.Turn.ID
			ev = core.NativeEvent{Type: core.EventRunCompleted}
		case "interrupted":
			r.state = protocol.StateCancelled
			r.nativeRef = "codex thread " + a.threadID + " turn " + p.Turn.ID
			ev = core.NativeEvent{Type: core.EventRunCancelled}
		default:
			r.state = protocol.StateFailed
			r.nativeRef = "codex thread " + a.threadID + " turn " + p.Turn.ID
			if p.Turn.Error != nil {
				r.errText = p.Turn.Error.Message
			}
			ev = core.NativeEvent{Type: core.EventRunFailed}
		}
		ev.Key, ev.RunID, ev.Result = r.key, r.runIDLocked(), r.result()
		r.interaction, r.queuedApprovals = nil, nil
		a.mu.Unlock()
		a.emit(ev)
	case "thread/status/changed":
		var p struct {
			ThreadID string `json:"threadId"`
			Status   struct {
				Type string `json:"type"`
			} `json:"status"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != a.threadID {
			return
		}
		a.mu.Lock()
		a.status = threadStatus(p.Status.Type)
		// notLoaded/systemError mean the app-server lost track of
		// any turn we previously observed (process restart, transcript not
		// loaded). No turn/completed will follow for that turn, and the
		// terminal memo is written only by turn/completed and lookupHistory —
		// so keeping the stale activeTurn would wedge Submit busy for the
		// life of the process. The observing status source is gone; drop it.
		// Gate on the RAW status type, NOT the mapped a.status: threadStatus
		// maps every unrecognised status to "unknown", and a future Codex
		// status meaning "still running" would then clear a LIVE turn's
		// activeTurn and Submit would issue turn/start into it. Only the
		// enumerated lost-track statuses clear; unrecognised statuses are
		// left untouched.
		switch p.Status.Type {
		case "notLoaded", "systemError":
			a.activeTurn = ""
			a.lostStateGen++
		case "idle":
			a.activeTurn = ""
		}
		a.mu.Unlock()
	}
}

func (a *Attachment) onItem(n Notification) {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Item     struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			ClientID string `json:"clientId"`
			Text     string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != a.threadID {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch p.Item.Type {
	case "userMessage":
		if r, ok := a.byClientID[p.Item.ClientID]; ok && p.Item.ClientID != "" {
			r.queued = false
			// Our text is confirmed in this turn: bind byTurn and mark owned.
			if a.confirmRun(r, p.TurnID) {
				go a.deliverPendingCancel(r.turnID)
			}
			return
		}
		// A user message we did not send landed in a turn we own: the human
		// steered locally.
		if r, ok := a.byTurn[p.TurnID]; ok && !r.local && n.Method == "item/started" {
			r.local = true
			key, runID := r.key, r.runIDLocked()
			go a.emit(core.NativeEvent{Type: core.EventLocalIntervention, Key: key, RunID: runID})
		}
	case "agentMessage":
		if r, ok := a.byTurn[p.TurnID]; ok && n.Method == "item/completed" {
			r.text.Reset()
			r.text.WriteString(p.Item.Text)
		}
	}
}

func (a *Attachment) onServerRequest(req ServerRequest) {
	switch req.Method {
	case methodCommandApproval, methodFileChangeApproval, methodLegacyExecApproval:
	case methodPermissionsApproval, methodToolUserInput, methodMcpElicitation:
		// These are shown, never dropped: otherwise a turn from the DM would
		// wait for a terminal answer the owner never heard of.
		a.questionMu.Lock()
		defer a.questionMu.Unlock()
		if turnID, inter := a.terminalQuestion(req); inter != nil {
			a.raiseApproval(turnID, req.ID, inter)
		}
		return
	default:
		return
	}
	a.questionMu.Lock()
	defer a.questionMu.Unlock()
	var p struct {
		ThreadID   string `json:"threadId"`
		TurnID     string `json:"turnId"`
		ItemID     string `json:"itemId"`
		ApprovalID string `json:"approvalId"`
		Command    any    `json:"command"`
		Cwd        string `json:"cwd"`
		Reason     string `json:"reason"`
		// Codex 0.160 mixes plain decisions with object decisions such as
		// {"acceptWithExecpolicyAmendment": ...}; a []string decode would
		// fail on the object and drop the whole request.
		AvailableDecisions []json.RawMessage `json:"availableDecisions"`
		// Fields a one-tap approve cannot show; any of them withholds it.
		Kind                  string          `json:"kind"`
		GrantRoot             string          `json:"grantRoot"`
		NetworkContext        json.RawMessage `json:"networkApprovalContext"`
		AdditionalPermissions json.RawMessage `json:"additionalPermissions"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.ThreadID != a.threadID {
		return
	}
	// The id names this server request, not only its item: Codex sends a
	// null approvalId for a shell approval and asks again under the same
	// itemId (a retry without the sandbox, network access), so the item id
	// alone would give the retry the closed approval's id and DM message.
	// The app-server resends a pending request under its own
	// request id, so the id is stable across a restart.
	id := cmp.Or(p.ApprovalID, p.ItemID) + ":" + requestPart(req.ID)
	// Only a plain decision can be sent back as {"decision": option}.
	var options []string
	for _, raw := range p.AvailableDecisions {
		var o string
		if json.Unmarshal(raw, &o) == nil {
			options = append(options, o)
		}
	}
	if len(p.AvailableDecisions) == 0 {
		options = []string{"accept", "decline"}
	}
	// Reject declines when Codex offers it; otherwise cancel, which declines
	// and stops the turn (Codex 0.160 offers no decline for some commands).
	reject := offered(options, "decline")
	if reject == "" {
		reject = offered(options, "cancel")
	}
	// The DM shows the display form, so that form is screened too, and an
	// approval that may hold a secret shows only that it waits.
	// Approve needs the shown text to be what runs.
	raw := approvalPrompt(req.Method, p.Command, p.Cwd, p.Reason)
	prompt := secretscan.DisplayForm(raw)
	shownWhole := prompt == raw
	if secretscan.MayHold(raw) || secretscan.MayHold(prompt) {
		prompt, shownWhole = "Codex asks for approval; it may hold a secret, so only the terminal shows it.", false
	}
	if reject == "cancel" {
		prompt += "\nReject also stops this turn."
	}
	// Approve is offered only for a plain command whose whole grant the
	// prompt shows. A file change shows no paths or diff here, and a
	// network, permission or write-root grant is not in the prompt, so those
	// are approved in the terminal; reject is always safe to offer.
	approve := ""
	if _, isText := p.Command.(string); req.Method == methodCommandApproval && isText &&
		(p.Kind == "" || p.Kind == "command") && p.GrantRoot == "" && isAbsent(p.NetworkContext) && isAbsent(p.AdditionalPermissions) &&
		shownWhole && len(prompt) <= protocol.MaxApprovalPreview {
		approve = offered(options, "accept")
	}
	a.raiseApproval(p.TurnID, req.ID, &protocol.Interaction{InteractionID: id, Kind: "approval", Prompt: prompt, Options: options,
		RemoteAnswer: true, ApproveOption: approve, RejectOption: reject})
}

// raiseApproval makes inter the pending interaction of the run that owns
// turnID, or queues it behind the pending one. An approval of a turn no run
// of ours owns is a thread approval. The caller holds questionMu.
func (a *Attachment) raiseApproval(turnID string, reqID json.RawMessage, inter *protocol.Interaction) {
	a.mu.Lock()
	if !a.approve {
		// Approvals not advertised: the local TUI answers.
		a.mu.Unlock()
		return
	}
	if key := compactID(reqID); a.resolvedEarly[key] {
		a.mu.Unlock()
		return // another client answered it before it reached us
	}
	r, ok := a.byTurn[turnID]
	if !ok {
		a.raiseThreadLocked(turnID, reqID, inter)
		return
	}
	r.approvalReqs[inter.InteractionID] = reqID
	if r.interaction != nil {
		r.queuedApprovals = append(r.queuedApprovals, inter)
		a.mu.Unlock()
		return
	}
	r.interaction = inter
	key, runID := r.key, r.runIDLocked()
	a.mu.Unlock()
	a.emit(core.NativeEvent{Type: core.EventQuestion, Key: key, RunID: runID, Interaction: inter})
}

// raiseThreadLocked records a decision approval of a turn no run of ours
// owns, such as one the user started in the TUI, and reports it. A request
// only the terminal answers is not reported: the owner can do nothing with
// it from Buzz. The caller holds questionMu and a.mu, which it releases.
func (a *Attachment) raiseThreadLocked(turnID string, reqID json.RawMessage, inter *protocol.Interaction) {
	k := threadKey(turnID, inter.InteractionID)
	if turnID == "" || len(inter.Options) == 0 || a.threadApprovals[k] != nil || len(a.threadApprovals) >= maxThreadApprovals {
		a.mu.Unlock()
		return
	}
	if a.threadApprovals == nil {
		a.threadApprovals = map[string]*threadApproval{}
	}
	ta := &threadApproval{turnID: turnID, reqID: reqID, inter: inter}
	a.threadApprovals[k] = ta
	epoch := a.epoch
	a.mu.Unlock()
	a.emit(core.NativeEvent{Type: core.EventThreadQuestion, Turn: turnID, Epoch: epoch, Interaction: inter})
}

// asks names what each terminal-only request asks for, without its text.
var asks = map[string]string{
	methodPermissionsApproval: "Codex asks for permissions",
	methodToolUserInput:       "Codex asks for input",
	methodMcpElicitation:      "An MCP server asks for input",
}

// terminalQuestion is the interaction for a request only the terminal can
// answer: what Codex asks, bounded, with no option, so the DM says to answer
// in the terminal and a remote answer is refused as not offered. It is nil
// for another thread or an unreadable request. Only the reason, question and
// message text is shown: a URL or a requested schema may hold more than the
// owner should see in a DM.
func (a *Attachment) terminalQuestion(req ServerRequest) (string, *protocol.Interaction) {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
		// item/permissions/requestApproval
		Cwd         string `json:"cwd"`
		Reason      string `json:"reason"`
		Permissions struct {
			FileSystem *struct {
				Entries []struct {
					Access string `json:"access"`
					Path   struct {
						Path    string `json:"path"`
						Pattern string `json:"pattern"`
						Value   struct {
							Kind string `json:"kind"`
						} `json:"value"`
					} `json:"path"`
				} `json:"entries"`
				Read  []string `json:"read"`
				Write []string `json:"write"`
			} `json:"fileSystem"`
			Network *struct {
				Enabled bool `json:"enabled"`
			} `json:"network"`
		} `json:"permissions"`
		// item/tool/requestUserInput
		Questions []struct {
			Header   string `json:"header"`
			Question string `json:"question"`
		} `json:"questions"`
		// mcpServer/elicitation/request
		ServerName string `json:"serverName"`
		Message    string `json:"message"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.ThreadID != a.threadID {
		return "", nil
	}
	id := p.ItemID
	var prompt string
	switch req.Method {
	case methodPermissionsApproval:
		var grants []string
		if fs := p.Permissions.FileSystem; fs != nil {
			for _, e := range fs.Entries {
				grants = append(grants, e.Access+" "+cmp.Or(e.Path.Path, e.Path.Pattern, e.Path.Value.Kind))
			}
			for _, path := range fs.Read {
				grants = append(grants, "read "+path)
			}
			for _, path := range fs.Write {
				grants = append(grants, "write "+path)
			}
		}
		if n := p.Permissions.Network; n != nil && n.Enabled {
			grants = append(grants, "network")
		}
		prompt = "Codex asks for permissions:\n" + approvalPrompt(req.Method, strings.Join(grants, "\n"), p.Cwd, p.Reason)
	case methodToolUserInput:
		prompt = "Codex asks for input:"
		for _, q := range p.Questions {
			if q.Header != "" {
				prompt += "\n" + q.Header + ": " + q.Question
			} else {
				prompt += "\n" + q.Question
			}
		}
	case methodMcpElicitation:
		// An elicitation has no item id, and its url-mode elicitationId is
		// the MCP server's own; the JSON-RPC request id is unique here.
		if json.Unmarshal(req.ID, &id) != nil {
			id = compactID(req.ID)
		}
		prompt = "MCP server " + p.ServerName + " asks: " + p.Message
	}
	if id == "" {
		return "", nil
	}
	// The asked text is the harness's or an MCP server's own words and may
	// echo a credential: then the DM shows only that something waits. The
	// DM shows the display form, so both are screened.
	raw := prompt
	prompt = secretscan.DisplayForm(raw)
	if secretscan.MayHold(raw) || secretscan.MayHold(prompt) {
		prompt = asks[req.Method] + "; it may hold a secret, so only the terminal shows it."
	}
	// The owner sees that a long request was cut, as protocol.BoundPreview shows it.
	if text, cut := protocol.TruncateText(prompt, protocol.MaxApprovalPreview); cut {
		prompt = text + " …[shortened]"
	}
	return p.TurnID, &protocol.Interaction{InteractionID: id, Kind: "approval", Prompt: prompt, RemoteAnswer: true}
}

// result returns the BOUNDED terminal evidence. The bound is owned HERE,
// at the source, so the endpoint's boundResult is a no-op and both sides
// digest the same bytes (the attachment and the endpoint must compute the
// ack digest over the same form — if the attachment digests the
// unbounded result and the endpoint digests the bounded one, any result
// larger than MaxResultBytes is never released).
//
// This is the ONLY place a protocol.Result is constructed in this
// package. lookupHistory does not build one directly — it populates the
// run's fields and calls result(), so the bounding point is one.
func (r *run) result() *protocol.Result {
	res := &protocol.Result{Text: r.text.String(), Error: r.errText}
	if r.nativeRef != "" {
		res.NativeRef = r.nativeRef
	}
	// Bound the raw text to MaxResultBytes (UTF-8-rune safe). The store's
	// MaxRecordBytes budget is derived from the worst-case JSON encoding
	// factor (6x) so a raw-bounded result always fits encoded.
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

// ThreadCwd is the working directory the app-server daemon at socketPath
// captured for thread: the session's own directory, not that of one of its
// tool calls.
func ThreadCwd(socketPath, thread string) (string, error) {
	// Request-only connection: no notifications are consumed.
	client, err := Dial(socketPath, Handlers{})
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": ClientName, "version": Version}}, nil); err != nil {
		return "", err
	}
	var res struct {
		Thread struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"thread"`
	}
	if err := client.Call(ctx, "thread/read", map[string]any{"threadId": thread}, &res); err != nil {
		return "", fmt.Errorf("thread/read %s: %w", thread, err)
	}
	if res.Thread.ID != thread || res.Thread.Cwd == "" {
		return "", fmt.Errorf("thread/read %s returned no working directory for it", thread)
	}
	return res.Thread.Cwd, nil
}

// LoadedThreads lists the threads the daemon currently has running, so the
// endpoint can attach to each of them. It opens a short-lived connection.
func LoadedThreads(socketPath string) ([]string, error) {
	return LoadedThreadsWithin(socketPath, 10*time.Second)
}

// LoadedThreadsWithin is LoadedThreads with its calls bounded by timeout.
func LoadedThreadsWithin(socketPath string, timeout time.Duration) ([]string, error) {
	// Request-only connection: no notifications are consumed.
	client, err := Dial(socketPath, Handlers{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := client.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": ClientName, "version": Version}}, nil); err != nil {
		return nil, err
	}
	var res struct {
		Data      []string `json:"data"`
		ThreadIDs []string `json:"threadIds"`
	}
	if err := client.Call(ctx, "thread/loaded/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	if len(res.Data) > 0 {
		return res.Data, nil
	}
	return res.ThreadIDs, nil
}
