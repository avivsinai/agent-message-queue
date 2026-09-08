package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// ClientName is what the attachment reports to the app-server.
const ClientName = "amq-remote"

// Version is stamped by the binary.
var Version = "dev"

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
	queued       bool
	state        protocol.State
	text         strings.Builder
	errText      string
	local        bool
	interaction  *protocol.Interaction
	approvalReqs map[string]json.RawMessage
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

	mu           sync.Mutex
	status       string
	activeTurn   string
	runs         map[requests.Key]*run
	byTurn       map[string]*run
	byClientID   map[string]*run
	cancelIntent map[requests.Key]bool
	listeners    map[int]func(core.NativeEvent)
	nextListener int
	offline      bool
}

// Option configures Attach.
type Option func(*Attachment)

// WithApprovals advertises approve_tool and answers command approvals from
// remote decisions. Off until fanout of approval requests to a second client
// is verified live.
func WithApprovals(on bool) Option { return func(a *Attachment) { a.approve = on } }

// Attach connects to the daemon socket, resumes threadID as a second client,
// and starts consuming its notifications.
func Attach(socketPath, threadID string, opts ...Option) (*Attachment, error) {
	client, err := Dial(socketPath)
	if err != nil {
		return nil, err
	}
	a := &Attachment{
		client:       client,
		threadID:     threadID,
		targetID:     TargetID(threadID),
		epoch:        fmt.Sprintf("cx-%d", time.Now().UnixNano()),
		status:       "unknown",
		runs:         map[requests.Key]*run{},
		byTurn:       map[string]*run{},
		byClientID:   map[string]*run{},
		cancelIntent: map[requests.Key]bool{},
		listeners:    map[int]func(core.NativeEvent){},
	}
	for _, o := range opts {
		o(a)
	}
	client.OnNotification = a.onNotification
	client.OnServerRequest = a.onServerRequest
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
	a.cwd = resumed.Thread.Cwd
	a.status = threadStatus(resumed.Thread.Status.Type)
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
	id := strings.ReplaceAll(threadID, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	return "codex:" + id
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
	var pending *string
	for _, r := range a.runs {
		if r.interaction != nil {
			id := r.interaction.InteractionID
			pending = &id
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
			Inspect: true, Submit: true, CancelRequest: true, Steer: true,
			ApproveTool: a.approve, AnswerQuestion: false, Terminal: "unavailable",
		},
		Evidence:   &protocol.Evidence{Submit: "admitted", Completion: "run_terminal"},
		ObservedAt: protocol.FormatTime(time.Now()),
	}
}

// Submit implements core.Attachment. The status check and the turn/start
// happen under one lock so a local turn starting in between is refused as
// busy instead of silently joining Codex's running turn (measured behavior).
func (a *Attachment) Submit(req core.BoundRequest) (core.Admission, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.offline {
		return core.Admission{}, errors.New("app-server connection is closed")
	}
	if req.Epoch != a.epoch {
		return core.Admission{Code: protocol.CodeStaleEpoch}, nil
	}
	if a.cancelIntent[req.Key] {
		delete(a.cancelIntent, req.Key)
		return core.Admission{Code: protocol.CodeCancelledBeforeAdmission}, nil
	}
	if existing, ok := a.runs[req.Key]; ok {
		return core.Admission{Admitted: true, RunID: existing.runID()}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	input := []map[string]string{{"type": "text", "text": req.Input.Text}}
	r := &run{key: req.Key, epoch: req.Epoch, state: protocol.StateRunning, approvalReqs: map[string]json.RawMessage{}}
	busy := a.status == "busy" || a.activeTurn != ""
	switch {
	case req.Input.Deliver == protocol.DeliverSteer:
		if !busy {
			return core.Admission{Code: protocol.CodeUnsupported, Message: "steer needs an active turn; use deliver=turn"}, nil
		}
		if err := a.client.Call(ctx, "turn/steer", map[string]any{"threadId": a.threadID, "expectedTurnId": a.activeTurn, "input": input, "clientUserMessageId": req.Key.RequestID}, nil); err != nil {
			return refusal(err), nil
		}
		r.turnID = a.activeTurn
	case busy && req.Input.Busy == protocol.BusyQueue:
		if err := a.client.Call(ctx, "thread/queue/add", map[string]any{"threadId": a.threadID, "clientUserMessageId": req.Key.RequestID, "input": input}, nil); err != nil {
			return refusal(err), nil
		}
		r.queued = true
	case busy:
		return core.Admission{Code: protocol.CodeBusy, Message: "a turn is active on this thread"}, nil
	default:
		var res struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := a.client.Call(ctx, "turn/start", map[string]any{"threadId": a.threadID, "input": input, "clientUserMessageId": req.Key.RequestID}, &res); err != nil {
			return refusal(err), nil
		}
		if res.Turn.ID == "" {
			return core.Admission{Code: protocol.CodeNativeError, Message: "turn/start returned no turn id"}, nil
		}
		if a.activeTurn != "" && a.activeTurn != res.Turn.ID {
			// Codex answered with a turn that is not the one we saw as active;
			// treat as a fresh turn and let notifications correct us.
			a.activeTurn = res.Turn.ID
		}
		r.turnID = res.Turn.ID
		a.activeTurn = res.Turn.ID
		a.status = "busy"
	}
	a.runs[req.Key] = r
	a.byClientID[req.Key.RequestID] = r
	if r.turnID != "" {
		a.byTurn[r.turnID] = r
	}
	return core.Admission{Admitted: true, RunID: r.runID()}, nil
}

func (r *run) runID() string {
	if r.turnID != "" {
		return "turn:" + r.turnID
	}
	return "queued:" + r.key.RequestID
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
		ev := core.Evidence{Known: true, Admitted: true, RunID: r.runID(), State: r.state, LocalIntervention: r.local, Interaction: r.interaction}
		if r.state.Terminal() {
			ev.Result = r.result()
		}
		a.mu.Unlock()
		return ev, nil
	}
	a.mu.Unlock()
	return a.lookupHistory(key)
}

func (a *Attachment) lookupHistory(key requests.Key) (core.Evidence, error) {
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
			if it.Type == "userMessage" && it.ClientID == key.RequestID {
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
		ev := core.Evidence{Known: true, Admitted: true, RunID: "turn:" + t.ID}
		switch t.Status {
		case "completed":
			ev.State = protocol.StateCompleted
			ev.Result = &protocol.Result{Text: text.String(), NativeRef: "codex thread " + a.threadID + " turn " + t.ID}
		case "failed":
			ev.State = protocol.StateFailed
			msg := ""
			if t.Error != nil {
				msg = t.Error.Message
			}
			ev.Result = &protocol.Result{Text: text.String(), Error: msg}
		case "interrupted":
			ev.State = protocol.StateCancelled
			ev.Result = &protocol.Result{Text: text.String()}
		default:
			ev.State = protocol.StateRunning
		}
		return ev, nil
	}
	return core.Evidence{}, nil
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
	turnID, queued := r.turnID, r.queued
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if queued && turnID == "" {
		if err := a.client.Call(ctx, "thread/queue/delete", map[string]any{"threadId": a.threadID, "clientUserMessageId": key.RequestID}, nil); err != nil {
			return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: err.Error()}, nil
		}
		a.mu.Lock()
		r.state = protocol.StateCancelled
		a.mu.Unlock()
		a.emit(core.NativeEvent{Type: core.EventRunCancelled, Key: key, RunID: r.runID()})
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
	delete(r.approvalReqs, interactionID)
	r.interaction = nil
	a.mu.Unlock()
	if err := a.client.Respond(reqID, map[string]string{"decision": option}); err != nil {
		return "", err
	}
	a.emit(core.NativeEvent{Type: core.EventQuestionResolved, Key: key, RunID: r.runID()})
	return "", nil
}

// AcknowledgeResult implements core.Attachment. Codex keeps the transcript;
// nothing is retained here beyond the process.
func (a *Attachment) AcknowledgeResult(requests.Key, string, string) {}

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
		r, ok := a.byTurn[p.Turn.ID]
		if !ok || r.state.Terminal() {
			a.mu.Unlock()
			return
		}
		var ev core.NativeEvent
		switch p.Turn.Status {
		case "completed":
			r.state = protocol.StateCompleted
			ev = core.NativeEvent{Type: core.EventRunCompleted}
		case "interrupted":
			r.state = protocol.StateCancelled
			ev = core.NativeEvent{Type: core.EventRunCancelled}
		default:
			r.state = protocol.StateFailed
			if p.Turn.Error != nil {
				r.errText = p.Turn.Error.Message
			}
			ev = core.NativeEvent{Type: core.EventRunFailed}
		}
		ev.Key, ev.RunID, ev.Result = r.key, r.runID(), r.result()
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
		if a.status == "idle" {
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
			if r.turnID == "" {
				r.turnID = p.TurnID
				r.queued = false
				a.byTurn[p.TurnID] = r
			}
			return
		}
		// A user message we did not send landed in a turn we own: the human
		// steered locally.
		if r, ok := a.byTurn[p.TurnID]; ok && !r.local && n.Method == "item/started" {
			r.local = true
			key, runID := r.key, r.runID()
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
	key, runID, inter := r.key, r.runID(), r.interaction
	a.mu.Unlock()
	a.emit(core.NativeEvent{Type: core.EventQuestion, Key: key, RunID: runID, Interaction: inter})
}

func (r *run) result() *protocol.Result {
	return &protocol.Result{Text: r.text.String(), Error: r.errText}
}

// LoadedThreads lists the threads the daemon currently has running, so the
// endpoint can attach to each of them. It opens a short-lived connection.
func LoadedThreads(socketPath string) ([]string, error) {
	client, err := Dial(socketPath)
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
