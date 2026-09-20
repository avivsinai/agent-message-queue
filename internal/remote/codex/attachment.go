package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
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

// Approval methods the app-server sends as server requests. Only the
// command-execution family is answered; the rest stay local-only.
const (
	methodCommandApproval    = "item/commandExecution/requestApproval"
	methodFileChangeApproval = "item/fileChange/requestApproval"
	methodLegacyExecApproval = "execCommand/approval"
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
	// createdAt is when the run was bound, for the unconfirmed-shadow
	// deadline (Pro F1): a retained-but-unconfirmed run shadows lookupHistory
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
	// of re-asking every tick (agent-message-queue-611.22.24).
	acked bool
}

// Attachment is one running Codex thread reached through the shared
// app-server daemon. It implements core.Attachment.
type Attachment struct {
	client   *Client
	threadID string
	targetID string
	epoch    string
	cwd      string
	approve  bool
	now      func() time.Time
	// confirmTimeout is how long Submit waits for our own userMessage item.
	// Overridable for tests.
	confirmTimeout time.Duration

	mu           sync.Mutex
	status       string
	activeTurn   string
	runs         map[requests.Key]*run
	byTurn       map[string]*run
	byClientID   map[string]*run // keyed by clientIDFor(key), not bare RequestID
	cancelIntent map[requests.Key]bool
	// terminalTurns is a bounded memo of turn IDs observed as terminal via
	// turn/completed, even when no byTurn entry exists (the confirming
	// userMessage item was missed in the race window). The RPC-response
	// guard consults this so a finished turn is never reinstalled as
	// active. Bounded via terminalTurnOrder (FIFO): when full, the OLDEST
	// observation is evicted — it is a race-window artefact, not a log
	// (B3, agent-message-queue-611.22.36).
	terminalTurns     map[string]bool
	terminalTurnOrder []string // FIFO of the same turn IDs; drives eviction
	// lostStateGen is bumped every time a notLoaded/systemError notification
	// tells us the app-server lost track of any previously-observed turn
	// (611.22.39-r2 F758-1). A turn/start RPC continuation captured the
	// generation BEFORE its call; if the generation advanced by the time the
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
	// bound memory (611.22.19 BK4), once the FIFO exceeds maxLiveRuns the
	// oldest acked run is fully dropped from all three maps. A later
	// Lookup/Cancel for it returns EvidenceNone/unknown exactly as a
	// compacted tombstone would; the durable store remains the source of
	// truth for the request's disposition.
	ackedOrder []requests.Key
	// ackedKeyOrder drives eviction of ackedKeys, the longer-lived memo that
	// lets a pruned run short-circuit Lookup. It outlives ackedOrder so a
	// compacted tombstone still converges (611.22.19 BK4).
	ackedKeyOrder []requests.Key
	// ackedKeys remembers which keys have been acknowledged, so a run pruned
	// from the bounded runs map (above) still short-circuits Lookup to
	// EvidenceNone instead of falling through to lookupHistory, whose copy
	// would look like fresh evidence and restart the ack loop. Bounded by the
	// same FIFO eviction as ackedOrder (611.22.19 BK4).
	ackedKeys map[requests.Key]bool
}

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

// WithClock overrides the attachment's clock (for tests). Production uses time.Now.
func WithClock(now func() time.Time) Option { return func(a *Attachment) { a.now = now } }

// WithConfirmTimeout overrides the confirm timeout (for tests).
func WithConfirmTimeout(d time.Duration) Option { return func(a *Attachment) { a.confirmTimeout = d } }

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
	a.client = client
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": ClientName, "version": Version, "title": "AMQ Remote"}}, nil); err != nil {
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
	a.mu.Lock()
	a.cwd = resumed.Thread.Cwd
	if a.activeTurn == "" {
		a.status = threadStatus(resumed.Thread.Status.Type)
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

// TargetID derives the stable target id for a thread.
func TargetID(threadID string) string {
	// The full de-hyphenated thread id, never truncated: two distinct thread
	// ids must not collapse to one target (Pro B03). Codex thread ids are
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
func (a *Attachment) Close() error { return a.client.Close() }

// Inspect implements core.Attachment.
func (a *Attachment) Inspect() protocol.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	att, status := "live", a.status
	if a.offline {
		att, status = "offline", "offline"
	}
	// yl0 (agent-message-queue-yl0; Inspect half of 611.22.51): ranging over
	// the runs map and keeping the last interaction picked whichever run the
	// randomizer visited last, so the published PendingInteraction flapped
	// between Inspect calls with no state change. The operator should answer
	// the interaction that has been waiting the longest: pick the OLDEST
	// interaction by run createdAt, ties broken by interaction id for full
	// determinism.
	var pending *string
	var pendingAt time.Time
	for _, r := range a.runs {
		if r.interaction == nil {
			continue
		}
		id := r.interaction.InteractionID
		if pending == nil || r.createdAt.Before(pendingAt) ||
			(r.createdAt.Equal(pendingAt) && id < *pending) {
			pending = &id
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

	input := []map[string]any{{"type": "text", "text": req.Input.Text}}

	switch {
	case req.Input.Deliver == protocol.DeliverSteer:
		// Steer interleaves into the running turn; expectedTurnId makes the
		// app-server reject if the active turn changed under us, so no
		// separate confirmation is needed.
		if !busy {
			return core.Admission{Code: protocol.CodeUnsupported, Message: "steer needs an active turn; use deliver=turn"}, nil
		}
		if err := a.call("turn/steer", map[string]any{"threadId": a.threadID, "expectedTurnId": activeTurn, "input": input, "clientUserMessageId": clientIDFor(req.Key)}, nil); err != nil {
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
		if err := a.call("thread/queue/add", map[string]any{"threadId": a.threadID, "clientUserMessageId": clientIDFor(req.Key), "input": input}, nil); err != nil {
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
	// 611.22.39-r2 F758-1: capture the lost-state generation before the RPC.
	// A notLoaded/systemError notification processed by the read pump while
	// this call is in flight means the app-server lost track of the turn we
	// are about to restore — the continuation must not resurrect it.
	a.mu.Lock()
	genBefore := a.lostStateGen
	a.mu.Unlock()
	if err := a.call("turn/start", map[string]any{"threadId": a.threadID, "input": input, "clientUserMessageId": clientIDFor(req.Key)}, &res); err != nil {
		// B1 (agent-message-queue-611.22.35): split pre-send vs post-send.
		// ErrNotSent (marshal error, write error, connection closed before
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
		// B1: a successful RPC response with no turn id is ambiguous, not a
		// definitive refusal. The server may have accepted the prompt under a
		// turn we have not observed yet. Preserve the correlation and return
		// a non-nil error so the endpoint records uncertain.
		return core.Admission{RunID: a.runID(r)}, fmt.Errorf("turn/start returned no turn id")
	}
	a.mu.Lock()
	// B3: the read pump may have already processed the confirming
	// notification, the completed result, and the final idle-status before
	// this goroutine resumes. Do not restore an already-finished turn as the
	// active turn — that would wedge the adapter permanently busy with no
	// later event to clear it. If the run is already terminal, OR the turn
	// was observed as terminal via the memo (confirming userMessage missed,
	// turn/completed recorded it), skip the activeTurn/status restoration.
	// 611.22.39-r2 F758-1: a notLoaded/systemError notification processed
	// while the RPC was in flight invalidated the state this continuation is
	// about to restore. The run is NOT terminal and the turn is NOT in the
	// memo (the lost-state notification does not make either true), so the
	// B3 guard above does not fire — the generation check is the only
	// defense. Stale = drop the restore; keep the run correlation (the text
	// may still be running server-side, so Lookup/history stay able to
	// resolve it) and report uncertain, exactly like the post-send
	// ambiguity path below.
	// BEAD e5q: the terminal check runs BEFORE the generation check. A
	// terminal state (r.state.Terminal() or a.terminalTurns hit) is a
	// POSITIVE fact that restores nothing — it only reports what already
	// happened — so a notLoaded/systemError in flight cannot invalidate it.
	// Ordered the other way, a lost-state notification that arrives while
	// the RPC is in flight downgrades a known-good completed outcome to
	// uncertain, and the endpoint then re-records evidence for a run that
	// was already proven finished.
	if r.state.Terminal() || a.terminalTurns[res.Turn.ID] {
		rid := r.runIDLocked()
		a.mu.Unlock()
		return core.Admission{Admitted: true, RunID: rid}, nil
	}
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
		// lookupHistory can still resolve it (agent-message-queue-611.22.31).
		// B1 (F3): runIDLocked takes a.mu — confirmRun writes r.turnID from the
		// read-loop goroutine. Every runID read now goes through the lock-taking
		// accessor (a.runID) or runIDLocked under a.mu.
		a.mu.Lock()
		rid := r.runIDLocked()
		a.mu.Unlock()
		return core.Admission{RunID: rid}, fmt.Errorf("turn/start was not confirmed by our own userMessage item within %s; the turn may be running", a.confirmTimeout)
	case <-a.client.Done():
		a.mu.Lock()
		rid := r.runIDLocked()
		a.mu.Unlock()
		return core.Admission{RunID: rid}, errors.New("app-server closed during turn/start; the turn may be running")
	}
}

// call runs one app-server request with a bounded timeout and no lock held.
func (a *Attachment) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return a.client.Call(ctx, method, params, result)
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
	// response is still in flight, reinstating the wedge this memo closes
	// (10b, packet 10 recut).
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
func (a *Attachment) deliverPendingCancel(key requests.Key, turnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.client.Call(ctx, "turn/interrupt", map[string]any{"threadId": a.threadID, "turnId": turnID}, nil); err != nil {
		// TODO(B04): a pending cancel that confirmed then raced completion is
		// lost here with no state mutation; re-record the intent or emit an
		// uncertain-cancel event so it is not silently dropped.
		return
	}
	// The resulting turn/completed(interrupted) drives the cancelled state.
	_ = key
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
// way to read a run's runID outside a.mu. B1 (F3): every read of a run's
// mutable fields (turnID, confirmed, state, text, createdAt) goes through a
// small accessor that takes a.mu; no caller touches r.<field> directly.
func (a *Attachment) runID(r *run) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return r.runIDLocked()
}

// clientIDFor returns the native correlation token for a request key. B2:
// the endpoint's identity is (CreatorHost, TargetID, RequestID), so two
// creator hosts can legitimately share a UUID. Indexing by bare RequestID
// would attribute one caller's failed run to another's live request. The
// composite token preserves the full namespace. This token is sent as
// clientUserMessageId to the server and echoed back in history items; the
// byClientID map is keyed by it.
func clientIDFor(key requests.Key) string {
	return protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
}

func refusal(err error) core.Admission {
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
			// Pro F1: a retained-but-unconfirmed run permanently shadows
			// lookupHistory, so the record sticks at Uncertain forever and a
			// real completed result is never delivered. After confirmTimeout,
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
	// 611.22.19 BK4: a run pruned from the bounded runs map after ack still
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
	if err := a.client.Call(ctx, "thread/read", map[string]any{"threadId": a.threadID, "includeTurns": true}, &res); err != nil {
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
		// B2: when history resolves a run as terminal, MAKE THE IN-MEMORY RUN
		// TERMINAL. Record the turn id so the correlation exists (byTurn), set
		// the terminal state, and populate the result text so AcknowledgeResult
		// can match the digest and release it. A run we have proven finished
		// must not stay "running" in our own map forever, re-reading the whole
		// transcript every tick (agent-message-queue-611.22.24).
		//
		// B3: r.result() is the ONLY place a protocol.Result is constructed in
		// this package — lookupHistory does not build one directly, so the
		// bounding point (MaxResultBytes, truncated) applies here too.
		nativeRef := "codex thread " + a.threadID + " turn " + t.ID
		if state.Terminal() {
			a.mu.Lock()
			// B3 (agent-message-queue-611.22.36): when history establishes a
			// turn terminal, clear a.activeTurn ONLY IF it is that same turn
			// id. If a newer turn is active, leave it alone — clearing a newer
			// turn is a worse bug.
			if a.activeTurn == t.ID {
				a.activeTurn = ""
				a.status = "idle"
			}
			// 10d (packet 10 recut): record through memoTerminal like every
			// other memo writer. A bare map write here bypassed the FIFO —
			// these entries never entered terminalTurnOrder, so eviction
			// (which measures the SLICE) never removed them and the map grew
			// without bound, one entry per distinct history-recovered turn.
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
				a.mu.Unlock()
				result := r.result()
				// 611.22.34 B2: this is HISTORY-proven terminal evidence — a
				// restarted attachment retains nothing in memory, so this
				// class tells the endpoint the outcome is the run's final
				// one and the ack replay may converge on it.
				return core.Evidence{Known: true, Admitted: true, Class: core.EvidenceHistoryTerminated, RunID: "turn:" + t.ID, State: state, Result: result}, nil
			}
			a.mu.Unlock()
		}
		// No in-memory run (e.g. after restart) or non-terminal: build the
		// result through the same helper so the bounding point is one.
		// B6: INSTALL a confirmed run entry so CancelExact can act on it.
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
			// B6 (agent-message-queue-611.22.36): a run installed by the
			// submit path always has an epoch, but a run recovered by an
			// earlier lookupHistory (before this fix) or by a concurrent
			// history call that lost the race may have an empty epoch.
			// Fill it from the requested epoch — history has proven this
			// is the same native turn, so the entry answers for the epoch
			// the endpoint holds.
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
			// B3: if this is a terminal turn recovered from history after
			// restart, clear a.activeTurn if it matches (the pump is dead).
			if state.Terminal() && a.activeTurn == t.ID {
				a.activeTurn = ""
				a.status = "idle"
			}
		}
		if t.ID != "" && r.turnID != "" {
			a.byTurn[r.turnID] = r
		}
		a.mu.Unlock()
		result := r.result()
		ev := core.Evidence{Known: true, Admitted: true, RunID: r.runIDLocked(), State: r.state, Result: result}
		if r.state.Terminal() {
			// 611.22.34 B2: same as above — history-proven terminal.
			ev.Class = core.EvidenceHistoryTerminated
		}
		return ev, nil
	}
	// No turn carried our clientId. That is NOT positive proof the request was
	// never admitted: the clientId echo is schema-backed but unverified
	// against a live app-server, and a single omitted field would otherwise
	// make the first reconcile after a restart REJECT every in-flight request
	// while its turns keep running. Absence of correlation is unknown —
	// uncertain, keep correlation — never EvidenceNone
	// (agent-message-queue-611.22.31).
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
		if err := a.client.Call(ctx, "thread/queue/delete", map[string]any{"threadId": a.threadID, "clientUserMessageId": clientIDFor(key)}, nil); err != nil {
			return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: err.Error()}, nil
		}
		a.mu.Lock()
		r.state = protocol.StateCancelled
		a.mu.Unlock()
		a.emit(core.NativeEvent{Type: core.EventRunCancelled, Key: key, RunID: a.runID(r)})
		return core.CancelEvidence{Disposition: protocol.CancelConfirmed}, nil
	}
	if err := a.client.Call(ctx, "turn/interrupt", map[string]any{"threadId": a.threadID, "turnId": turnID}, nil); err != nil {
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
	a.mu.Unlock()
	if err := a.client.Respond(reqID, map[string]string{"decision": option}); err != nil {
		return "", err
	}
	// B8a (agent-message-queue-611.22.36): tear down the approval state ONLY
	// after Respond succeeds. Previously the delete + nil happened before the
	// send, so a transport failure left r.interaction == nil and a retry
	// returned CodeAlreadyResolved while the native question was still open.
	a.mu.Lock()
	delete(r.approvalReqs, interactionID)
	r.interaction = nil
	a.mu.Unlock()
	a.emit(core.NativeEvent{Type: core.EventQuestionResolved, Key: key, RunID: a.runID(r)})
	return "", nil
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
	// 611.22.19 BK4: bound the in-memory correlation maps. A terminal+acked
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

func (a *Attachment) onNotification(n Notification) {
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
	case "item/started", "item/completed":
		a.onItem(n)
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
		a.mu.Lock()
		if a.activeTurn == p.Turn.ID {
			a.activeTurn = ""
			a.status = "idle"
		}
		// B3 (agent-message-queue-611.22.36): record the terminal observation
		// even when no byTurn entry exists (the confirming userMessage item
		// was missed). The RPC-response guard consults this memo so a finished
		// turn is never reinstalled as active.
		// 10a (packet 10 recut): record UNCONDITIONALLY. The notification means
		// the turn is over — every arm of the status switch below (including
		// default -> StateFailed) produces a terminal state, so keeping a
		// narrower status whitelist here re-derives terminality a second time
		// and wedges again for any status it fails to enumerate.
		// 10b (packet 10 recut): the bound is owned by memoTerminal (FIFO
		// via terminalTurnOrder — Go map iteration order is randomized, so
		// walking the map to "evict the oldest" evicts an arbitrary entry,
		// possibly the turn whose RPC response is still in flight).
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
		r.interaction = nil
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
		// 611.22.39: notLoaded/systemError mean the app-server lost track of
		// any turn we previously observed (process restart, transcript not
		// loaded). No turn/completed will follow for that turn, and the
		// terminal memo is written only by turn/completed and lookupHistory —
		// so keeping the stale activeTurn wedged Submit busy for the life of
		// the process. The observing status source is gone; drop it.
		// Gate on the RAW status type, NOT the mapped a.status: threadStatus
		// maps every unrecognised status to "unknown", and a future Codex
		// status meaning "still running" would then clear a LIVE turn's
		// activeTurn and Submit would issue turn/start into it (verifier
		// round-1 blocker). Only the enumerated lost-track statuses clear;
		// unrecognised statuses are left untouched.
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
				key := r.key
				turnID := r.turnID
				go a.deliverPendingCancel(key, turnID)
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
	default:
		return
	}
	var p struct {
		ThreadID           string   `json:"threadId"`
		TurnID             string   `json:"turnId"`
		ItemID             string   `json:"itemId"`
		ApprovalID         string   `json:"approvalId"`
		Command            any      `json:"command"`
		AvailableDecisions []string `json:"availableDecisions"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.ThreadID != a.threadID {
		return
	}
	a.mu.Lock()
	r, ok := a.byTurn[p.TurnID]
	if !ok || !a.approve {
		// Not our run, or approvals not advertised: the local TUI answers.
		a.mu.Unlock()
		return
	}
	id := p.ApprovalID
	if id == "" {
		id = p.ItemID
	}
	options := p.AvailableDecisions
	if len(options) == 0 {
		options = []string{"accept", "decline"}
	}
	prompt := ""
	if b, err := json.Marshal(p.Command); err == nil {
		prompt = string(b)
	}
	r.interaction = &protocol.Interaction{InteractionID: id, Kind: "approval", Prompt: prompt, Options: options, RemoteAnswer: true}
	r.approvalReqs[id] = req.ID
	key, runID, inter := r.key, r.runIDLocked(), r.interaction
	a.mu.Unlock()
	a.emit(core.NativeEvent{Type: core.EventQuestion, Key: key, RunID: runID, Interaction: inter})
}

// result returns the BOUNDED terminal evidence. The bound is owned HERE,
// at the source, so the endpoint's boundResult is a no-op and both sides
// digest the same bytes (Pro F2: the attachment and the endpoint must
// compute the ack digest over the same form — if the attachment digests the
// unbounded result and the endpoint digests the bounded one, any result
// larger than MaxResultBytes is never released).
//
// B3: this is the ONLY place a protocol.Result is constructed in this
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

// LoadedThreads lists the threads the daemon currently has running, so the
// endpoint can attach to each of them. It opens a short-lived connection.
func LoadedThreads(socketPath string) ([]string, error) {
	// Request-only connection: no notifications are consumed.
	client, err := Dial(socketPath, Handlers{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
