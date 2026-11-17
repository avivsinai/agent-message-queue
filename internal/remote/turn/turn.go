// Package turn runs one prompt turn against a native target through the
// amq-remote endpoint: submit, busy retry, follow, cancel, and how the turn
// reports. It knows no JSON-RPC and no Buzz: a turn reports through the
// function values its caller fills in.
package turn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Outcome is how a turn settled. Its value is the reason on the wire.
type Outcome string

// The outcomes of a turn. Open is a turn that is not settled yet.
const (
	Open         Outcome = ""
	Replied      Outcome = "replied"
	Cancelled    Outcome = "session_cancelled"
	Disconnected Outcome = "client_disconnected"
	TimedOut     Outcome = "reply_timeout"
)

// Stopped reports a turn the client ended (a cancel or a closed stream)
// before it was answered or timed out.
func (o Outcome) Stopped() bool {
	return o == Cancelled || o == Disconnected
}

// Stop reasons of a turn's result.
const (
	// StopReasonEndTurn ends a turn that has its answer.
	StopReasonEndTurn = "end_turn"
	// StopReasonRefusal ends a turn without an answer.
	StopReasonRefusal = "refusal"
	// StopReasonCancelled ends a turn the client cancelled.
	StopReasonCancelled = "cancelled"
)

// Request states a turn reports when the endpoint has no record to name.
const (
	// StateNotSubmitted is a request that does not exist.
	StateNotSubmitted = "not_submitted"
	// StateUncertain is a request whose existence or outcome is unknown.
	StateUncertain = "uncertain"
)

// Delivery states of a prompt delivered as an AMQ message.
const (
	// StateQueued is a prompt in the recipient's inbox/new; consumption is
	// a separate, unproven event.
	StateQueued = "queued_to_inbox"
	// StateReplied is a prompt answered by a fresh reply.
	StateReplied = "replied"
)

// Post kinds: each event posts at most one status text (a timeout, a stop
// that leaves work running, a failure to submit) and at most one final
// answer. Separate kinds keep an earlier status post from suppressing the
// answer that a later delivery of the event brings.
const (
	PostStatus = "status"
	PostFinal  = "final"
)

// remoteAdmitWithin bounds how long the endpoint may still admit a prompt.
// It matches the amq-remote submit default.
const remoteAdmitWithin = 2 * time.Minute

// busyRetryPause is the gap between busy submit retries. A 10-minute turn
// budget must not become thousands of submit IPCs.
const busyRetryPause = time.Second

// Result is a prompt turn run against one amq-remote target. The turn
// submits into the live native session; it writes no AMQ message.
type Result struct {
	StopReason string     `json:"stopReason"`
	Meta       ResultMeta `json:"_meta"`
	// CancelledBeforeAdmit is set when a cancel stopped a queued turn before
	// the endpoint admitted it. The caller that keeps a durable cancel
	// records it; nothing was submitted.
	CancelledBeforeAdmit bool `json:"-"`
}

// ResultMeta is the result's _meta object.
type ResultMeta struct {
	Remote Meta `json:"remote"`
}

// Meta reports what the endpoint recorded. State is the request's recorded
// state, or not_submitted when no request exists, or uncertain when its
// existence is unknown. Cancel is the endpoint's cancel disposition or
// refusal code, set only when the client cancelled a live request.
type Meta struct {
	Target     string `json:"target"`
	RequestRef string `json:"requestRef,omitempty"`
	State      string `json:"state,omitempty"`
	Code       string `json:"code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Cancel     string `json:"cancel,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	// Posted is "posted" when the owner-facing text was published into the
	// Buzz channel, "duplicate: ..." when an earlier delivery or the
	// late-reply sweep already posted it, or the post error.
	Posted string `json:"posted,omitempty"`
}

// Turn is one prompt turn against the pinned target. The caller fills the
// exported fields before Run. Dir, Target and Native are fixed when the turn
// starts, so a later rebind never moves work that is already submitted.
type Turn struct {
	// Name prefixes the turn's stderr lines.
	Name string
	// Dir is the endpoint's state directory, <root>/extensions/remote.
	Dir string
	// Target is the endpoint target, and Native the native session pin
	// every command carries.
	Target, Native string
	// EventID is the owner's event id, or "" for a prompt with none.
	EventID string
	// Timing is the turn's durations.
	Timing Timing
	// Done is closed when the turn is settled from outside.
	Done <-chan struct{}
	// Settle decides the turn's outcome if it is still open and returns the
	// outcome that stands. Open only reads it.
	Settle func(Outcome) Outcome
	// Emit shows one text to the ACP client: update is
	// "agent_thought_chunk" or "agent_message_chunk". Mirror relays one
	// native session update. Both are nil when there is no client.
	Emit   func(update, text string) error
	Mirror func(update json.RawMessage) error
	// Post publishes the turn's status or final text (kind PostStatus or
	// PostFinal) to its Buzz channel and returns the Meta.Posted text; nil
	// when there is no channel.
	Post func(kind, text string) string
	// MailboxThread is a mailbox turn's AMQ thread. A failure text points at
	// it, not at amq-remote status.
	MailboxThread string
	// Meta is what the turn reports; Run sets its target and request.
	Meta Meta
	// quiet posts the final answer without emitting it again: its text
	// already reached the activity view as streamed native activity.
	quiet bool
}

type remoteWait struct {
	resp *ipc.Response
	err  error
}

// Run submits text to the pinned amq-remote target and holds the turn open
// until the request reaches a terminal or uncertain state. Every command
// carries the pinned native session, so a replacement session behind the
// same target is refused. A cancel asks the endpoint to cancel that exact
// request, and its answer is reported: an adapter that cannot cancel never
// reads as cancelled work.
func (t *Turn) Run(text string) (Result, error) {
	id, err := RequestID(t.EventID)
	if err != nil {
		return Result{}, fmt.Errorf("request id: %v", err)
	}
	// The exact reference exists before any IPC, so an unknown submit
	// outcome still names the request.
	t.Meta = Meta{Target: t.Target, RequestRef: protocol.EncodeRef(ipc.LocalHost, t.Target, id)}

	// A redelivered event follows its stored request, whatever the target's
	// epoch is now. A busy-rejected record is resubmitted
	// under its stored epoch, so the endpoint's own retry rule decides it. A
	// failed lookup proves nothing about absence.
	epoch := ""
	if t.EventID != "" {
		rep, err := t.get()
		switch {
		case err == nil && retryableBusy(rep.Snapshot):
			epoch = rep.Snapshot.Epoch
		case err == nil:
			return t.follow(rep.Snapshot)
		case !hasCode(err, protocol.CodeNotFound):
			return t.Failed(StateUncertain, err)
		}
	}
	label := t.Target
	if epoch == "" {
		session, err := remoteSession(t.Dir, t.Target, t.Native)
		if err != nil {
			return t.Failed(StateNotSubmitted, err)
		}
		epoch = session.Epoch
		if session.DisplayName != "" {
			label = session.DisplayName
		}
	}
	// Dir, Target and Native stay fixed for every retry, so a rebind during
	// the wait cannot move this turn.
	deadline := time.Now().Add(t.Timing.Timeout)
	queued := false
	var last protocol.Snapshot
	for {
		if outcome := t.Settle(Open); outcome.Stopped() {
			return t.stoppedBeforeAdmit(outcome, last)
		}
		rep, err := t.call(&protocol.Command{
			Schema:    protocol.SchemaCommand,
			Op:        protocol.OpRequestSubmit,
			RequestID: id,
			TargetID:  t.Target,
			Epoch:     epoch,
			NotAfter:  protocol.FormatTime(time.Now().Add(remoteAdmitWithin)),
			Input:     &protocol.SubmitInput{Text: text, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn},
		})
		if err != nil {
			// Unreachable and unshared refuse before the endpoint sees the
			// command. Any other error may follow a native run, so the exact
			// record decides.
			if hasCode(err, protocol.CodeEndpointUnreachable) || hasCode(err, protocol.CodeUnshared) {
				return t.Failed(StateNotSubmitted, err)
			}
			stored, gerr := t.get()
			switch {
			case gerr == nil && retryableBusy(stored.Snapshot):
				// A lost reply to a submit the endpoint refused as busy: the
				// stored tombstone is retried like a busy reply.
				rep = stored
				rep.Outcome.Code = protocol.CodeBusy
			case gerr == nil:
				return t.follow(stored.Snapshot)
			case hasCode(gerr, protocol.CodeNotFound):
				return t.Failed(StateNotSubmitted, err)
			default:
				return t.Failed(StateUncertain, err)
			}
		}
		if rep.Outcome.Code == protocol.CodeBusy {
			last = rep.Snapshot
			if rep.Snapshot.Epoch != "" {
				epoch = rep.Snapshot.Epoch
			}
			if !queued {
				queued = true
				if err := t.emit("agent_thought_chunk", fmt.Sprintf("Queued: %s is busy", label)); err != nil {
					return Result{}, fmt.Errorf("emit ACP session update: %v", err)
				}
			}
			switch stop := t.waitBusy(deadline); stop {
			case Open:
				continue
			case TimedOut:
				return t.settled(t.Settle(TimedOut), last)
			default:
				return t.stoppedBeforeAdmit(stop, last)
			}
		}
		if rep.Outcome.Code != "" {
			t.Meta.Code, t.Meta.Reason = string(rep.Outcome.Code), rep.Outcome.Message
			t.Meta.State = string(rep.Snapshot.State)
			if outcome := t.Settle(Replied); outcome != Replied {
				return t.settled(outcome, rep.Snapshot)
			}
			return t.Say(snapKind(rep.Snapshot), Replied, StopReasonRefusal, t.statusText(rep.Snapshot))
		}
		return t.follow(rep.Snapshot)
	}
}

// stoppedBeforeAdmit ends a queued turn that was never admitted. It does not
// submit again and does not ask the endpoint to cancel: a busy tombstone
// never started a native run. A cancel is marked on the result, so the caller
// that keeps a durable cancel records it.
func (t *Turn) stoppedBeforeAdmit(outcome Outcome, snap protocol.Snapshot) (Result, error) {
	if snap.RequestRef != "" {
		t.Meta.State, t.Meta.Code = string(snap.State), string(snap.Code)
	}
	t.Meta.Reason = string(outcome)
	if outcome == Disconnected {
		return Result{StopReason: StopReasonRefusal, Meta: ResultMeta{Remote: t.Meta}}, nil
	}
	return Result{StopReason: StopReasonCancelled, Meta: ResultMeta{Remote: t.Meta}, CancelledBeforeAdmit: outcome == Cancelled}, nil
}

// waitBusy pauses between busy retries. Open means retry. The
// turn budget and a cancel both stop the loop without another submit.
func (t *Turn) waitBusy(deadline time.Time) Outcome {
	pause := busyRetryPause
	if remain := time.Until(deadline); remain < pause {
		pause = remain
	}
	if pause <= 0 {
		return TimedOut
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-t.Done:
		return t.Settle(Open)
	case <-timer.C:
		if !time.Now().Before(deadline) {
			return TimedOut
		}
		return ""
	}
}

// emit shows text to the client, if there is one.
func (t *Turn) emit(update, text string) error {
	if t.Emit == nil {
		return nil
	}
	return t.Emit(update, text)
}

// follow waits on the endpoint until the request is terminal or uncertain,
// the client cancels or leaves, or the turn times out.
func (t *Turn) follow(snap protocol.Snapshot) (Result, error) {
	if settledState(snap) {
		return t.settled(t.Settle(Replied), snap)
	}
	if err := t.emit("agent_thought_chunk", fmt.Sprintf("Submitted to %s as %s.", t.Meta.Target, snap.RequestRef)); err != nil {
		return Result{}, fmt.Errorf("emit ACP session update: %v", err)
	}
	mirror := t.startMirror(snap.RequestRef)
	defer mirror.finish(0)

	// The goroutine reads only these immutable values; snap belongs to the
	// consumer loop below. It waits past each revision it saw, so a pending
	// interaction ends a wait, and stops once the
	// request is terminal or uncertain.
	ref, native, timeout, after := snap.RequestRef, t.Native, t.Timing.Heartbeat.Milliseconds(), snap.Revision
	waits := make(chan remoteWait, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			resp, err := ipc.Call(t.Dir, ipc.Request{Wait: &ipc.WaitRequest{RequestRef: ref, TimeoutMS: timeout, AfterRevision: &after}, NativeSession: native})
			select {
			case waits <- remoteWait{resp, err}:
			case <-stop:
				return
			}
			if err != nil || resp.Error != nil {
				return
			}
			var current protocol.Snapshot
			if json.Unmarshal(resp.Reply, &current) != nil || !resp.TimedOut && settledState(current) {
				return
			}
			after = max(after, current.Revision)
		}
	}()

	deadline := time.NewTimer(t.Timing.Timeout)
	defer deadline.Stop()
	for {
		select {
		case <-t.Done:
			mirror.finish(0)
			return t.settled(t.Settle(Open), snap)
		case <-deadline.C:
			mirror.finish(0)
			return t.settled(t.Settle(TimedOut), snap)
		case w := <-waits:
			if w.err == nil {
				w.err = w.resp.AsError()
			}
			if w.err != nil {
				mirror.finish(0)
				return t.Failed(StateUncertain, w.err)
			}
			var current protocol.Snapshot
			if err := json.Unmarshal(w.resp.Reply, &current); err != nil {
				return Result{}, fmt.Errorf("decode request snapshot: %v", err)
			}
			snap = current
			if w.resp.TimedOut {
				if err := t.emit("agent_thought_chunk", fmt.Sprintf("Still running on %s.", t.Meta.Target)); err != nil {
					return Result{}, fmt.Errorf("emit ACP heartbeat: %v", err)
				}
				continue
			}
			if !settledState(snap) {
				continue // a newer revision of a running request
			}
			// The endpoint ends the stream right after the request settles,
			// so the streamed activity precedes the answer.
			outcome := t.Settle(Replied)
			streamed := mirror.finish(mirrorSettle)
			t.quiet = snap.State == protocol.StateCompleted && snap.Result != nil && echoed(streamed, snap.Result.Text)
			return t.settled(outcome, snap)
		}
	}
}

// mirrorSettle bounds the wait for the activity stream to end after the
// request settled. mirrorTailBytes is how much of the latest streamed agent
// text a turn keeps to compare with its answer.
const (
	mirrorSettle    = 2 * time.Second
	mirrorTailBytes = 64 << 10
)

// echoed reports whether the streamed agent text already ends with the
// answer, so the activity view shows it. A longer answer, a truncated one, or
// a stream that dropped its end is sent again rather than lost.
func echoed(streamed, answer string) bool {
	answer = strings.TrimSpace(answer)
	return answer != "" && strings.HasSuffix(strings.TrimSpace(streamed), answer)
}

// mirroredUpdates are the session updates a native activity stream may
// carry to the client. Anything else, such as session state a client acts
// on, is dropped.
var mirroredUpdates = map[string]bool{"agent_message_chunk": true, "tool_call": true, "tool_call_update": true, "plan": true}

// remoteMirror relays the native activity of one running request to the
// client: streamed text, tool calls and plan updates, read from the
// endpoint's activity stream under the
// turn's native pin. The stream is read-only, and only session updates
// leave it, so it can never submit, cancel or approve. A
// stream that fails, or an endpoint without one, leaves the turn as it was.
type remoteMirror struct {
	cancel context.CancelFunc
	done   chan struct{}
	// tail is the end of the streamed agent text, written only by the
	// stream goroutine and read after it is done.
	tail []byte
}

func (t *Turn) startMirror(ref string) *remoteMirror {
	ctx, cancel := context.WithCancel(context.Background())
	m := &remoteMirror{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		req := ipc.Request{Activity: &ipc.ActivityRequest{RequestRef: ref}, NativeSession: t.Native}
		_ = ipc.Activity(ctx, t.Dir, req, func(update json.RawMessage) {
			var u struct {
				SessionUpdate string `json:"sessionUpdate"`
			}
			if ctx.Err() != nil || json.Unmarshal(update, &u) != nil || !mirroredUpdates[u.SessionUpdate] {
				return
			}
			if t.Mirror == nil || t.Mirror(update) != nil {
				cancel() // the client is gone; the turn reports that itself
				return
			}
			var text struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if u.SessionUpdate == "agent_message_chunk" && json.Unmarshal(update, &text) == nil {
				m.tail = append(m.tail, text.Content.Text...)
				if over := len(m.tail) - mirrorTailBytes; over > 0 {
					m.tail = append(m.tail[:0], m.tail[over:]...)
				}
			}
		})
	}()
	return m
}

// finish waits up to wait for the stream to end by itself, then stops it and
// waits for its last emission, so nothing is emitted after the turn's
// response. It returns the end of the streamed agent text.
func (m *remoteMirror) finish(wait time.Duration) string {
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-m.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	m.cancel()
	<-m.done
	return string(m.tail)
}

// settledState reports a terminal or uncertain request: no later revision
// changes what the turn reports.
func settledState(snap protocol.Snapshot) bool {
	return snap.State.Terminal() || snap.State == protocol.StateUncertain
}

// settled renders the turn once its outcome stands. The first settled
// outcome wins on every path: a cancel that won is reported even when the
// request later completed.
func (t *Turn) settled(outcome Outcome, snap protocol.Snapshot) (Result, error) {
	t.Meta.State, t.Meta.Code = string(snap.State), string(snap.Code)
	switch outcome {
	case Cancelled:
		return t.cancel(snap)
	case Disconnected:
		t.Meta.Reason = string(outcome)
		return Result{StopReason: StopReasonRefusal, Meta: ResultMeta{Remote: t.Meta}}, nil
	case TimedOut:
		t.Meta.Reason = string(outcome)
		return t.Say(PostStatus, outcome, StopReasonRefusal, fmt.Sprintf("%s: request %s is still %s and the work continues. Check it with `amq-remote status %s`.", t.Meta.Target, snap.RequestRef, snap.State, snap.RequestRef))
	}
	switch snap.State {
	case protocol.StateCompleted:
		text := ""
		if snap.Result != nil {
			text, t.Meta.Truncated = snap.Result.Text, snap.Result.Truncated
		}
		return t.Say(PostFinal, outcome, StopReasonEndTurn, text)
	case protocol.StateCancelled:
		return t.Say(PostFinal, outcome, StopReasonCancelled, t.statusText(snap))
	default: // failed, rejected, uncertain
		if snap.State == protocol.StateRejected && t.Meta.Reason == "" {
			// A deferred refusal keeps the adapter's text on the record,
			// not the snapshot; request.get carries it.
			if rep, err := t.get(); err == nil {
				t.Meta.Reason = rep.Outcome.Message
			}
		}
		return t.Say(snapKind(snap), outcome, StopReasonRefusal, t.statusText(snap))
	}
}

// retryableBusy reports a busy refusal, which a later delivery retries.
func retryableBusy(snap protocol.Snapshot) bool {
	return snap.State == protocol.StateRejected && snap.Code == protocol.CodeBusy
}

// snapKind is PostFinal only for a terminal request state that no later
// delivery retries (a busy refusal is retried). An uncertain
// request can still resolve to an answer, so its notice is a status text
// and never takes the final-answer marker.
func snapKind(snap protocol.Snapshot) string {
	if snap.State.Terminal() && !retryableBusy(snap) {
		return PostFinal
	}
	return PostStatus
}

// cancel asks the endpoint to cancel the exact request under the epoch it was
// stored with. The disposition comes from the reply's outcome or, for the
// usual path, its snapshot. Work that keeps running is said so.
func (t *Turn) cancel(snap protocol.Snapshot) (Result, error) {
	t.Meta.Reason = string(Cancelled)
	if snap.State.Terminal() {
		return Result{StopReason: StopReasonCancelled, Meta: ResultMeta{Remote: t.Meta}}, nil
	}
	rep, err := t.call(&protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestCancel,
		RequestRef: snap.RequestRef,
		TargetID:   snap.TargetID,
		Epoch:      snap.Epoch,
		NotAfter:   protocol.FormatTime(time.Now().Add(remoteAdmitWithin)),
	})
	switch {
	case err != nil:
		t.Meta.Cancel = codeOf(err)
	case rep.Outcome.Code != "":
		t.Meta.Cancel = string(rep.Outcome.Code)
	default:
		disposition := rep.Outcome.Disposition
		if disposition == "" && rep.Snapshot.Cancel != nil {
			disposition = rep.Snapshot.Cancel.Disposition
		}
		t.Meta.Cancel = string(disposition)
	}
	if err == nil && rep.Snapshot.State != "" {
		t.Meta.State = string(rep.Snapshot.State)
	}
	if t.Meta.State == string(protocol.StateCancelled) {
		return Result{StopReason: StopReasonCancelled, Meta: ResultMeta{Remote: t.Meta}}, nil
	}
	return t.Say(PostStatus, Cancelled, StopReasonCancelled, fmt.Sprintf("%s: cancel %s; request %s is still %s and the work continues.", t.Meta.Target, t.Meta.Cancel, snap.RequestRef, t.Meta.State))
}

// Failed ends a turn whose request is absent (StateNotSubmitted) or unknown
// (StateUncertain). The exact reference stays in the result, so an unknown
// outcome is never an invitation to submit again.
func (t *Turn) Failed(state string, err error) (Result, error) {
	t.Meta.State, t.Meta.Code, t.Meta.Reason = state, codeOf(err), err.Error()
	var refusal *protocol.Refusal
	if errors.As(err, &refusal) {
		t.Meta.Reason = refusal.Message
	}
	if state == StateNotSubmitted {
		t.Meta.RequestRef = ""
	}
	outcome := t.Settle(Replied)
	if outcome == Disconnected {
		t.Meta.Reason = string(outcome)
		return Result{StopReason: StopReasonRefusal, Meta: ResultMeta{Remote: t.Meta}}, nil
	}
	stopReason := StopReasonRefusal
	if outcome == Cancelled {
		stopReason = StopReasonCancelled
	}
	text := fmt.Sprintf("%s: not submitted (%s): %s", t.Meta.Target, t.Meta.Code, protocol.InertInline(t.Meta.Reason))
	switch {
	case t.MailboxThread != "" && state == StateUncertain:
		text = fmt.Sprintf("%s: the delivery outcome is unknown: %s. Do not resend; check it with `amq thread --id %s`.", t.Meta.Target, protocol.InertInline(t.Meta.Reason), t.MailboxThread)
	case state == StateUncertain:
		text = fmt.Sprintf("%s: the outcome of request %s is unknown: %s. Do not resend; check it with `amq-remote status %s`.", t.Meta.Target, t.Meta.RequestRef, protocol.InertInline(t.Meta.Reason), t.Meta.RequestRef)
	}
	return t.Say(PostStatus, outcome, stopReason, text)
}

// Say posts text and shows it as the agent message, then returns the result.
// A client that left gets no message. kind is PostStatus or PostFinal.
func (t *Turn) Say(kind string, outcome Outcome, stopReason, text string) (Result, error) {
	if outcome != Disconnected && text == "" && kind == PostFinal {
		fmt.Fprintf(os.Stderr, "%s: %s ended with no final text; nothing was posted\n", t.Name, t.Meta.Target)
	}
	if outcome != Disconnected && text != "" {
		// Buzz first: a failed ACP emission never skips the post.
		if t.Post != nil {
			t.Meta.Posted = t.Post(kind, text)
		}
		if kind == PostFinal && strings.HasPrefix(t.Meta.Posted, "error") {
			fmt.Fprintf(os.Stderr, "%s: the final answer of %s was not posted: %s\n", t.Name, t.Meta.Target, t.Meta.Posted)
		}
		if t.quiet && kind == PostFinal {
			return Result{StopReason: stopReason, Meta: ResultMeta{Remote: t.Meta}}, nil
		}
		if err := t.emit("agent_message_chunk", text); err != nil {
			return Result{}, fmt.Errorf("emit ACP reply update: %v", err)
		}
	}
	return Result{StopReason: stopReason, Meta: ResultMeta{Remote: t.Meta}}, nil
}

func (t *Turn) statusText(snap protocol.Snapshot) string {
	text := fmt.Sprintf("%s: request %s", t.Meta.Target, t.Meta.State)
	if t.Meta.Code != "" {
		text += " (" + t.Meta.Code + ")"
	}
	if t.Meta.Reason != "" {
		text += ": " + protocol.InertInline(t.Meta.Reason)
	} else if snap.Result != nil && snap.Result.Error != "" {
		text += ": " + protocol.InertInline(snap.Result.Error)
	}
	return text
}

func (t *Turn) get() (protocol.Reply, error) {
	return t.call(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: t.Meta.RequestRef})
}

func (t *Turn) call(cmd *protocol.Command) (protocol.Reply, error) {
	resp, err := ipc.Call(t.Dir, ipc.Request{Command: cmd, NativeSession: t.Native})
	if err != nil {
		return protocol.Reply{}, err
	}
	if err := resp.AsError(); err != nil {
		return protocol.Reply{}, err
	}
	var rep protocol.Reply
	if err := json.Unmarshal(resp.Reply, &rep); err != nil {
		return protocol.Reply{}, err
	}
	return rep, nil
}

func remoteSession(dir, target, native string) (protocol.Session, error) {
	resp, err := ipc.Call(dir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: target}, NativeSession: native})
	if err != nil {
		return protocol.Session{}, err
	}
	if err := resp.AsError(); err != nil {
		return protocol.Session{}, err
	}
	var session protocol.Session
	if err := json.Unmarshal(resp.Reply, &session); err != nil {
		return protocol.Session{}, err
	}
	return session, nil
}

func hasCode(err error, code protocol.Code) bool {
	var refusal *protocol.Refusal
	return errors.As(err, &refusal) && refusal.Code == code
}

func codeOf(err error) string {
	var refusal *protocol.Refusal
	if errors.As(err, &refusal) {
		return string(refusal.Code)
	}
	return "error"
}

// RequestID is the endpoint request id for one prompt. A Nostr event id
// maps to a fixed id, so a redelivered event reconciles with its first submit
// instead of running twice.
func RequestID(eventID string) (string, error) {
	var b [16]byte
	if eventID != "" {
		sum := sha256.Sum256([]byte("amq-acp remote request:" + eventID))
		copy(b[:], sum[:16])
	} else if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
