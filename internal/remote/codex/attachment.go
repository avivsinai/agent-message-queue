package codex

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
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

// Requests the app-server sends to every client of a thread. Each one is
// opened as a dialog for the binding that serves the thread; the first
// answer wins, from Buzz or from the Codex terminal.
const (
	methodCommandApproval     = "item/commandExecution/requestApproval"
	methodFileChangeApproval  = "item/fileChange/requestApproval"
	methodLegacyExecApproval  = "execCommand/approval"
	methodPermissionsApproval = "item/permissions/requestApproval"
	methodToolUserInput       = "item/tool/requestUserInput"
	methodMcpElicitation      = "mcpServer/elicitation/request"
	// methodServerRequestResolved tells every client on the thread that a
	// server request was answered, by whichever client answered first.
	methodServerRequestResolved = "serverRequest/resolved"
)

type run struct {
	key       requests.Key
	epoch     string
	turnID    string
	state     protocol.State
	text      strings.Builder
	errText   string
	nativeRef string
	local     bool
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
	now      func() time.Time
	// codexHome is the daemon's Codex home that the thread's rollout path
	// proves, or "" when thread/resume did not report one.
	codexHome string
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
	// resolvedEarly holds request ids that serverRequest/resolved named
	// before the request worker delivered the request itself; such a
	// request is already answered and is never shown. seen holds ids the
	// worker delivered whose resolution has not arrived, so a resolution of
	// a dialog already closed is not mistaken for an early one. An id
	// leaves either set when its pair arrives, so a later request that
	// reuses it is shown.
	resolvedEarly, seen idSet
	// dialogs are the open server requests by compactID of their JSON-RPC
	// id, each waiting for its first answer. At most maxDialogs; past that
	// the terminal answers alone.
	dialogs map[string]*openDialog
	// dialogWG counts the running dialog waiters, so Close returns only
	// after each one wrote its resolution.
	dialogWG sync.WaitGroup
	// fileChanges are the in-progress fileChange items by item id, so a
	// file change approval, which names only its item, shows the paths and
	// the diff. At most maxFileChanges, oldest dropped first.
	fileChanges     map[string]string
	fileChangeOrder []string
}

// maxIDs bounds an idSet.
const maxIDs = 64

// idSet is a set of JSON-RPC ids that drops its oldest entry past maxIDs.
type idSet struct {
	has   map[string]bool
	order []string
}

func (s *idSet) add(id string) {
	if s.has[id] {
		return
	}
	if s.has == nil {
		s.has = map[string]bool{}
	}
	s.has[id] = true
	s.order = append(s.order, id)
	for len(s.has) > maxIDs {
		delete(s.has, s.order[0])
		s.order = s.order[1:]
	}
	if len(s.order) > 2*maxIDs {
		// Drop the order entries of ids taken since they were added.
		kept, in := s.order[:0], map[string]bool{}
		for _, o := range s.order {
			if s.has[o] && !in[o] {
				kept, in[o] = append(kept, o), true
			}
		}
		s.order = kept
	}
}

// take reports whether id is in the set and removes it.
func (s *idSet) take(id string) bool {
	if !s.has[id] {
		return false
	}
	delete(s.has, id)
	return true
}

// maxDialogs bounds Attachment.dialogs.
const maxDialogs = 64

// maxFileChanges bounds Attachment.fileChanges.
const maxFileChanges = 64

// Option configures Attach.
type Option func(*Attachment)

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
	// Without the experimental API, Codex strips additionalPermissions from
	// approval requests and sends no request_user_input, so the dialog
	// would show less than the terminal does.
	init := map[string]any{"clientInfo": map[string]string{"name": ClientName, "version": Version, "title": "AMQ Remote"},
		"capabilities": map[string]bool{"experimentalApi": true}}
	if err := client.Call(ctx, "initialize", init, nil); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var resumed struct {
		Thread struct {
			ID     string `json:"id"`
			Cwd    string `json:"cwd"`
			Path   string `json:"path"`
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
	a.codexHome = codexHomeFromRollout(resumed.Thread.Path, threadID)
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
		a.endDialogs("")
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

// Close drops the connection and resolves the open dialogs as expired
// before it returns. The thread keeps running in Codex.
func (a *Attachment) Close() error {
	err := a.client.Load().Close()
	a.endDialogs("")
	a.dialogWG.Wait()
	return err
}

// Inspect implements core.Attachment.
func (a *Attachment) Inspect() protocol.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	att, status := "live", a.status
	if a.offline {
		att, status = "offline", "offline"
	}
	return protocol.Session{
		Schema:           protocol.SchemaSession,
		TargetID:         a.targetID,
		Epoch:            a.epoch,
		Harness:          "codex",
		DisplayName:      "codex " + a.threadID,
		Project:          a.cwd,
		Attachment:       att,
		Status:           status,
		ApprovalReviewer: a.reviewer,
		// Approvals are answered through dialogs, never through Respond.
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: true, CancelRequest: true, Steer: false,
			ApproveTool: false, AnswerQuestion: false, Terminal: "unavailable",
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
	activeTurn, codexHome := a.activeTurn, a.codexHome
	a.mu.Unlock()
	// A turn sent while the seat's TUI still shows a startup screen would
	// end the hold that keeps the seat's own doorbell out of that screen.
	if held, why := seatStartupHeld(a.threadID, codexHome); held {
		return core.Admission{Code: protocol.CodeBusy, Message: why, NothingSent: true}, nil
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
		r := &run{key: req.Key, epoch: req.Epoch, state: protocol.StateRunning, turnID: activeTurn, confirmed: true, createdAt: a.now()}
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
		r := &run{key: req.Key, epoch: req.Epoch, state: protocol.StateRunning, queued: true, createdAt: a.now()}
		a.mu.Lock()
		a.runs[req.Key] = r
		a.byClientID[clientIDFor(req.Key)] = r
		a.mu.Unlock()
		return core.Admission{Admitted: true, RunID: a.runID(r)}, nil

	case busy:
		return core.Admission{Code: protocol.CodeBusy, Message: "a turn is active on this thread", NothingSent: true}, nil
	}

	// Idle turn/start. Register the run first so onItem can confirm our
	// userMessage, then call unlocked, then require confirmation.
	r := &run{key: req.Key, epoch: req.Epoch, state: protocol.StateRunning, confirmedCh: make(chan struct{}), createdAt: a.now()}
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
		ev := core.Evidence{Known: true, RunID: r.runIDLocked(), State: r.state, LocalIntervention: r.local}
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

// Respond implements core.Attachment. No run interaction is ever raised:
// every server request is answered through its dialog, so there is nothing
// for the endpoint to answer here.
func (a *Attachment) Respond(requests.Key, string, string, string) (protocol.Code, error) {
	return protocol.CodeAlreadyResolved, nil
}

// onServerRequestResolved closes the dialog of a request that a client
// answered. The app-server drops every answer after the first without an
// error, so a dialog the terminal answered is resolved as terminal; one our
// own answer closed is resolved by the dialog's waiter.
func (a *Attachment) onServerRequestResolved(n Notification) {
	var p struct {
		ThreadID  string          `json:"threadId"`
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != a.threadID || len(p.RequestID) == 0 {
		return
	}
	key := compactID(p.RequestID)
	if key == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.seen.take(key) {
		// Server requests reach onServerRequest through a worker queue, so
		// the resolution can overtake its own request. Remember it.
		a.resolvedEarly.add(key)
		return
	}
	if d := a.dialogs[key]; d != nil {
		if d.sending {
			d.resolvedWhileSending = true
			return
		}
		d.endLocked(dialog.OutcomeTerminal, dialog.ByTerminal)
	}
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
		a.endDialogs(p.Turn.ID)
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
			Type     string       `json:"type"`
			ID       string       `json:"id"`
			ClientID string       `json:"clientId"`
			Text     string       `json:"text"`
			Changes  []fileChange `json:"changes"`
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
	case "fileChange":
		if n.Method == "item/completed" {
			delete(a.fileChanges, p.Item.ID)
			return
		}
		if a.fileChanges == nil {
			a.fileChanges = map[string]string{}
		}
		if _, seen := a.fileChanges[p.Item.ID]; !seen {
			a.fileChangeOrder = append(a.fileChangeOrder, p.Item.ID)
		}
		a.fileChanges[p.Item.ID] = fileChangeText(p.Item.Changes)
		for len(a.fileChangeOrder) > maxFileChanges {
			delete(a.fileChanges, a.fileChangeOrder[0])
			a.fileChangeOrder = a.fileChangeOrder[1:]
		}
	case "agentMessage":
		if r, ok := a.byTurn[p.TurnID]; ok && n.Method == "item/completed" {
			r.text.Reset()
			r.text.WriteString(p.Item.Text)
		}
	}
}

func (a *Attachment) onServerRequest(req ServerRequest) {
	key := compactID(req.ID)
	if key == "" {
		return
	}
	a.mu.Lock()
	early := a.resolvedEarly.take(key)
	if !early {
		a.seen.add(key)
	}
	a.mu.Unlock()
	d := a.dialogFor(req)
	if early || d == nil {
		return // another client answered it before it reached us, or no dialog
	}
	a.mu.Lock()
	if a.dialogs[key] != nil || len(a.dialogs) >= maxDialogs {
		// Already open, or past the bound: the terminal answers it alone.
		a.mu.Unlock()
		return
	}
	if a.dialogs == nil {
		a.dialogs = map[string]*openDialog{}
	}
	a.dialogs[key] = d
	a.dialogWG.Add(1)
	a.mu.Unlock()
	go a.awaitDialog(key, d)
}

// openDialog is one server request of the thread, open as a dialog until
// its first answer. The fields after done are guarded by Attachment.mu.
type openDialog struct {
	reqID  json.RawMessage
	turnID string
	req    dialog.Request
	// reply maps the owner's answer to the app-server response, the
	// resolution outcome and the chosen label; ok=false when the answer
	// fits no choice of this request.
	reply func(dialog.Answer) (result any, outcome, label string, ok bool)
	// done closes once, when the request ended without our answer; outcome
	// and by say how.
	done        chan struct{}
	outcome, by string
	// sending is set while our answer is being sent. A resolution that
	// arrives meanwhile may be our own answer's, so the waiter decides.
	sending, resolvedWhileSending bool
}

// endLocked closes the dialog with how it ended, once.
func (d *openDialog) endLocked(outcome, by string) {
	if d.outcome != "" {
		return
	}
	d.outcome, d.by = outcome, by
	close(d.done)
}

// endDialogs ends the open dialogs of turnID, or every one for "", as
// expired: the turn ended or the connection closed while they were open.
func (a *Attachment) endDialogs(turnID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, d := range a.dialogs {
		if (turnID == "" || d.turnID == turnID) && !d.sending {
			d.endLocked(dialog.OutcomeExpired, dialog.ByTimeout)
		}
	}
}

// dialogPoll is how often a waiting dialog looks for the owner's answer.
const dialogPoll = 250 * time.Millisecond

// awaitDialog opens d for the binding that serves this thread, waits for
// the owner's answer or for the request to end elsewhere, and answers the
// app-server with the owner's choice. The first resolution written wins.
func (a *Attachment) awaitDialog(key string, d *openDialog) {
	defer a.dialogWG.Done()
	defer func() {
		a.mu.Lock()
		if a.dialogs[key] == d {
			delete(a.dialogs, key)
		}
		a.mu.Unlock()
	}()
	// A thread no binding serves answers its dialogs in the terminal: it
	// runs in the app-server daemon and has no process of its own to prove
	// it live for a channel.
	name, err := dialog.FindBinding("", "", a.threadID)
	if err != nil || name == "" {
		return
	}
	dir, err := dialog.DirFor(name)
	if err != nil {
		return
	}
	d.req.OpenedAt = a.now().UTC()
	if dialog.Open(dir, d.req) != nil {
		return
	}
	tick := time.NewTicker(dialogPoll)
	defer tick.Stop()
	for {
		select {
		case <-d.done:
			a.mu.Lock()
			outcome, by := d.outcome, d.by
			a.mu.Unlock()
			_, _ = dialog.Resolve(dir, dialog.Resolved{ID: d.req.ID, Outcome: outcome, By: by, At: a.now().UTC()})
			return
		case <-tick.C:
		}
		ans, ok, err := dialog.ReadAnswer(dir, d.req.ID)
		if err != nil || !ok {
			continue
		}
		result, outcome, label, ok := d.reply(ans)
		if !ok {
			// The answer fits no choice. Remove it, so the owner's next
			// reply can be written, and keep waiting.
			_ = dialog.RemoveAnswer(dir, d.req.ID)
			continue
		}
		a.mu.Lock()
		if d.outcome != "" {
			a.mu.Unlock()
			continue // ended meanwhile; the done case resolves it
		}
		d.sending = true
		a.mu.Unlock()
		sendErr := a.client.Load().Respond(d.reqID, result)
		a.mu.Lock()
		elsewhere := d.resolvedWhileSending
		d.sending = false
		a.mu.Unlock()
		r := dialog.Resolved{ID: d.req.ID, Outcome: outcome, Label: label, By: dialog.ByBuzz, At: a.now().UTC()}
		switch {
		case sendErr != nil && elsewhere:
			// Another client answered while our send failed.
			r = dialog.Resolved{ID: d.req.ID, Outcome: dialog.OutcomeTerminal, By: dialog.ByTerminal, At: r.At}
		case sendErr != nil:
			r = dialog.Resolved{ID: d.req.ID, Outcome: dialog.OutcomeExpired, By: dialog.ByTimeout, At: r.At}
		}
		_, _ = dialog.Resolve(dir, r)
		return
	}
}

// maxDialogText bounds the text of one dialog; the terminal shows the rest.
const maxDialogText = 64 << 10

// dialogFor is the dialog of one server request of this thread: what Codex
// shows in its terminal, with Codex's own choices in Codex's order. It is
// nil for another thread, an unreadable request, or a request that is not
// a dialog.
func (a *Attachment) dialogFor(req ServerRequest) *openDialog {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
		Cwd      string `json:"cwd"`
		Reason   string `json:"reason"`
		// item/commandExecution/requestApproval, item/fileChange/requestApproval
		Command        any    `json:"command"`
		GrantRoot      string `json:"grantRoot"`
		NetworkContext *struct {
			Host     string `json:"host"`
			Protocol string `json:"protocol"`
		} `json:"networkApprovalContext"`
		AdditionalPermissions json.RawMessage `json:"additionalPermissions"`
		// Codex mixes plain decisions with object decisions such as
		// {"acceptWithExecpolicyAmendment": ...}.
		AvailableDecisions []json.RawMessage `json:"availableDecisions"`
		// item/permissions/requestApproval
		Permissions json.RawMessage `json:"permissions"`
		// item/tool/requestUserInput
		Questions []userQuestion `json:"questions"`
		// mcpServer/elicitation/request
		ServerName      string          `json:"serverName"`
		Mode            string          `json:"mode"`
		Message         string          `json:"message"`
		URL             string          `json:"url"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
	}
	if json.Unmarshal(req.Params, &p) != nil || p.ThreadID != a.threadID {
		return nil
	}
	d := &openDialog{reqID: req.ID, turnID: p.TurnID, done: make(chan struct{}),
		req: dialog.Request{ID: dialog.NewID(), Harness: "codex", Session: a.threadID, PID: os.Getpid()}}
	var b strings.Builder
	line := func(format string, args ...any) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, format, args...)
	}
	where := func() {
		if p.Cwd != "" {
			line("in %s", p.Cwd)
		}
		if p.Reason != "" {
			line("reason: %s", p.Reason)
		}
	}
	switch req.Method {
	case methodCommandApproval:
		d.req.Tool = "exec"
		switch c := p.Command.(type) {
		case string:
			line("$ %s", c)
		case nil:
		default:
			if raw, err := json.Marshal(c); err == nil {
				line("$ %s", raw)
			}
		}
		where()
		if n := p.NetworkContext; n != nil {
			line("network: %s %s", n.Protocol, n.Host)
		}
		if p.GrantRoot != "" {
			line("write access to %s", p.GrantRoot)
		}
		if g := grantsText(p.AdditionalPermissions); g != "" {
			line("additional permissions:\n%s", g)
		}
		d.req.Options, d.reply = decisions(p.AvailableDecisions, "accept", "decline")
	case methodFileChangeApproval:
		d.req.Tool = "apply_patch"
		a.mu.Lock()
		changes := a.fileChanges[p.ItemID]
		a.mu.Unlock()
		if changes != "" {
			line("%s", changes)
		} else {
			line("apply a file change")
		}
		where()
		if p.GrantRoot != "" {
			line("write access to %s", p.GrantRoot)
		}
		d.req.Options, d.reply = decisions(p.AvailableDecisions, "accept", "acceptForSession", "decline", "cancel")
	case methodPermissionsApproval:
		d.req.Tool = "permissions"
		line("Codex asks for permissions:")
		if g := grantsText(p.Permissions); g != "" {
			line("%s", g)
		}
		where()
		d.req.Options, d.reply = permissionChoices(p.Permissions)
	case methodToolUserInput:
		d.req.Tool = "request_user_input"
		for _, q := range p.Questions {
			if q.Header != "" {
				line("%s: %s", q.Header, q.Question)
			} else {
				line("%s", q.Question)
			}
			for _, o := range q.Options {
				if o.Description != "" {
					line("- %s: %s", o.Label, o.Description)
				} else {
					line("- %s", o.Label)
				}
			}
		}
		if len(p.Questions) > 1 {
			line("Reply with one line per question, in order.")
		}
		d.req.Options, d.req.FreeText, d.reply = userInputChoices(p.Questions)
	case methodMcpElicitation:
		d.req.Tool = "mcp_elicitation"
		line("MCP server %s asks: %s", p.ServerName, p.Message)
		if p.URL != "" {
			line("%s", p.URL)
		}
		if len(p.RequestedSchema) > 0 && string(p.RequestedSchema) != "null" {
			var pretty bytes.Buffer
			if json.Indent(&pretty, p.RequestedSchema, "", "  ") == nil {
				line("form:\n%s", pretty.String())
			}
		}
		d.req.Options, d.req.FreeText, d.reply = elicitationChoices(p.Mode, p.RequestedSchema)
	default:
		return nil
	}
	text := b.String()
	if t, cut := protocol.TruncateText(text, maxDialogText); cut {
		text = t + "\n… full text in the terminal"
	}
	d.req.Text = text
	return d
}

// number gives options their 1-based numbers in order.
func number(options []dialog.Option) []dialog.Option {
	for i := range options {
		options[i].N = i + 1
	}
	return options
}

// chosen is the option an answer picked, or false.
func chosen(options []dialog.Option, ans dialog.Answer) (dialog.Option, bool) {
	for _, o := range options {
		if ans.Option != 0 && o.N == ans.Option {
			return o, true
		}
	}
	return dialog.Option{}, false
}

// decisions are a command or file change approval's choices: Codex's
// availableDecisions in Codex's order, or fallback when Codex lists none.
// An option's Value is the decision's JSON, which is sent back as it came.
// Deny is decline, or cancel when Codex offers no decline.
func decisions(raws []json.RawMessage, fallback ...string) ([]dialog.Option, func(dialog.Answer) (any, string, string, bool)) {
	if len(raws) == 0 {
		for _, f := range fallback {
			raw, _ := json.Marshal(f)
			raws = append(raws, raw)
		}
	}
	var options []dialog.Option
	deny := -1
	for _, raw := range raws {
		value := compactID(raw)
		if value == "" {
			continue
		}
		label := decisionLabel(raw)
		var tok string
		if json.Unmarshal(raw, &tok) == nil {
			value = tok
			if tok == "decline" || tok == "cancel" && deny < 0 {
				deny = len(options)
			}
		}
		options = append(options, dialog.Option{Label: label, Value: value})
	}
	if deny >= 0 {
		options[deny].Deny = true
	}
	options = number(options)
	return options, func(ans dialog.Answer) (any, string, string, bool) {
		o, ok := chosen(options, ans)
		if !ok {
			return nil, "", "", false
		}
		decision := json.RawMessage(o.Value)
		if !json.Valid(decision) || !strings.HasPrefix(o.Value, "{") {
			decision, _ = json.Marshal(o.Value)
		}
		outcome := dialog.OutcomeAllowed
		if o.Value == "decline" || o.Value == "cancel" || strings.Contains(o.Value, `"action":"deny"`) {
			outcome = dialog.OutcomeDenied
		}
		return map[string]json.RawMessage{"decision": decision}, outcome, o.Label, true
	}
}

// decisionLabel is how the dialog names one Codex decision.
func decisionLabel(raw json.RawMessage) string {
	var tok string
	if json.Unmarshal(raw, &tok) == nil {
		switch tok {
		case "accept":
			return "Allow once"
		case "acceptForSession":
			return "Allow for session"
		case "decline":
			return "Deny"
		case "cancel":
			return "Cancel turn"
		}
		return tok
	}
	var obj struct {
		Exec *struct {
			Amendment []string `json:"execpolicy_amendment"`
		} `json:"acceptWithExecpolicyAmendment"`
		Network *struct {
			Amendment struct {
				Action string `json:"action"`
				Host   string `json:"host"`
			} `json:"network_policy_amendment"`
		} `json:"applyNetworkPolicyAmendment"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		switch {
		case obj.Exec != nil:
			return "Allow, and don't ask again for commands that start with: " + strings.Join(obj.Exec.Amendment, " ")
		case obj.Network != nil:
			return "Always " + obj.Network.Amendment.Action + " " + obj.Network.Amendment.Host
		}
	}
	return compactID(raw)
}

// permissionChoices grant the requested permissions for this turn or for
// the session, or grant none.
func permissionChoices(requested json.RawMessage) ([]dialog.Option, func(dialog.Answer) (any, string, string, bool)) {
	options := number([]dialog.Option{
		{Label: "Allow for this turn", Value: "turn"},
		{Label: "Allow for session", Value: "session"},
		{Label: "Deny", Value: "deny", Deny: true},
	})
	if len(requested) == 0 || string(requested) == "null" {
		requested = json.RawMessage(`{}`)
	}
	return options, func(ans dialog.Answer) (any, string, string, bool) {
		o, ok := chosen(options, ans)
		switch {
		case !ok:
			return nil, "", "", false
		case o.Deny:
			return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, dialog.OutcomeDenied, o.Label, true
		}
		return map[string]any{"permissions": requested, "scope": o.Value}, dialog.OutcomeAllowed, o.Label, true
	}
}

// userQuestion is one request_user_input question.
type userQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

// userInputChoices answer request_user_input. A single question's choices
// are options; any reply text is an answer too: the whole text for a single
// question, one line per question, in order, for several.
func userInputChoices(questions []userQuestion) ([]dialog.Option, bool, func(dialog.Answer) (any, string, string, bool)) {
	var options []dialog.Option
	if len(questions) == 1 {
		for _, o := range questions[0].Options {
			options = append(options, dialog.Option{Label: o.Label, Value: o.Label})
		}
	}
	options = number(options)
	answers := func(texts []string) any {
		out := map[string]any{}
		for i, q := range questions {
			text := ""
			if i < len(texts) {
				text = strings.TrimSpace(texts[i])
			}
			out[q.ID] = map[string][]string{"answers": {text}}
		}
		return map[string]any{"answers": out}
	}
	return options, true, func(ans dialog.Answer) (any, string, string, bool) {
		if o, ok := chosen(options, ans); ok {
			return answers([]string{o.Value}), dialog.OutcomeAnswered, o.Label, true
		}
		if strings.TrimSpace(ans.Text) == "" || len(questions) == 0 {
			return nil, "", "", false
		}
		texts := []string{ans.Text}
		if len(questions) > 1 {
			texts = strings.Split(ans.Text, "\n")
		}
		return answers(texts), dialog.OutcomeAnswered, "", true
	}
}

// elicitationChoices answer an MCP elicitation: accept, decline or cancel,
// as Codex offers them. A form also takes reply text as its content: a JSON
// object as it is, or the value of the form's only field.
func elicitationChoices(mode string, schema json.RawMessage) ([]dialog.Option, bool, func(dialog.Answer) (any, string, string, bool)) {
	options := number([]dialog.Option{
		{Label: "Accept", Value: "accept"},
		{Label: "Decline", Value: "decline", Deny: true},
		{Label: "Cancel", Value: "cancel"},
	})
	form := mode != "url" && mode != "openai/userVerification"
	var fields struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(schema, &fields)
	return options, form, func(ans dialog.Answer) (any, string, string, bool) {
		if o, ok := chosen(options, ans); ok {
			res := map[string]any{"action": o.Value}
			outcome := dialog.OutcomeDenied
			if o.Value == "accept" {
				outcome = dialog.OutcomeAllowed
				if form {
					res["content"] = map[string]any{}
				}
			}
			return res, outcome, o.Label, true
		}
		text := strings.TrimSpace(ans.Text)
		if !form || text == "" {
			return nil, "", "", false
		}
		var content map[string]any
		if json.Unmarshal([]byte(text), &content) != nil {
			if len(fields.Properties) != 1 {
				return nil, "", "", false
			}
			for name := range fields.Properties {
				content = map[string]any{name: text}
			}
		}
		return map[string]any{"action": "accept", "content": content}, dialog.OutcomeAnswered, "", true
	}
}

// grantsText lists a permission profile's grants, one per line; the
// profile's JSON when it names none this reads.
func grantsText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var p struct {
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
			Enabled *bool `json:"enabled"`
		} `json:"network"`
	}
	var grants []string
	if json.Unmarshal(raw, &p) == nil {
		if fs := p.FileSystem; fs != nil {
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
		if n := p.Network; n != nil && n.Enabled != nil && *n.Enabled {
			grants = append(grants, "network")
		}
	}
	if len(grants) == 0 {
		if c := compactID(raw); c != "{}" {
			return c
		}
		return ""
	}
	return strings.Join(grants, "\n")
}

// fileChangeText is a fileChange item's paths and diffs, as the terminal
// shows them before the change applies.
func fileChangeText(changes []fileChange) string {
	var b strings.Builder
	for _, c := range changes {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		switch {
		case c.Kind.Type == "update" && c.Kind.MovePath != "":
			fmt.Fprintf(&b, "update %s -> %s", c.Path, c.Kind.MovePath)
		default:
			fmt.Fprintf(&b, "%s %s", cmp.Or(c.Kind.Type, "change"), c.Path)
		}
		if c.Diff != "" {
			b.WriteByte('\n')
			b.WriteString(strings.TrimRight(c.Diff, "\n"))
		}
	}
	return b.String()
}

// fileChange is one change of a fileChange item.
type fileChange struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
	Kind struct {
		Type     string `json:"type"`
		MovePath string `json:"move_path"`
	} `json:"kind"`
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
