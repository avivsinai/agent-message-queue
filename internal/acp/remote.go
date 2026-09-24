package acp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// remoteStateDir is amq-remote's state directory under the queue root
// (cmd/amq-remote stateDirName). The endpoint's owner-only socket lives there.
const remoteStateDir = "extensions/remote"

// remoteAdmitWithin bounds how long the endpoint may still admit a prompt.
// It matches the amq-remote submit default.
const remoteAdmitWithin = 2 * time.Minute

// remotePromptResult is a prompt turn run against one amq-remote target. The
// turn submits into the live native session; it writes no AMQ message.
type remotePromptResult struct {
	StopReason string           `json:"stopReason"`
	Meta       remotePromptMeta `json:"_meta"`
}

type remotePromptMeta struct {
	Remote remoteMeta `json:"remote"`
}

// remoteMeta reports what the endpoint recorded. State is the request's
// recorded state, or not_submitted when no request exists, or uncertain when
// its existence is unknown. Cancel is the endpoint's cancel disposition or
// refusal code, set only when the client cancelled a live request.
type remoteMeta struct {
	Target     string `json:"target"`
	RequestRef string `json:"requestRef,omitempty"`
	State      string `json:"state,omitempty"`
	Code       string `json:"code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Cancel     string `json:"cancel,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

const (
	remoteNotSubmitted = "not_submitted"
	remoteUncertain    = "uncertain"
)

type remoteWait struct {
	resp *ipc.Response
	err  error
}

// remoteTurn is one prompt turn against the pinned target. Its root,
// target and native pin are fixed when the turn starts, so a later rebind
// never moves work that is already submitted.
type remoteTurn struct {
	s         *Server
	dir       string
	target    string
	native    string
	sessionID string
	emit      func(any) error
	turn      *turnState
	meta      remoteMeta
}

// runRemote submits the prompt to the pinned amq-remote target and holds the
// turn open until the request reaches a terminal or uncertain state. Every
// command carries the pinned native session, so a replacement session behind
// the same target is refused. A cancel asks the endpoint to cancel that exact
// request, and its answer is reported: an adapter that cannot cancel never
// reads as cancelled work.
func (s *Server) runRemote(sessionID, text, eventID string, turn *turnState, emit func(any) error) (any, *rpcError) {
	id, err := remoteRequestID(eventID)
	if err != nil {
		return nil, newRPCError(codeInternalError, "request id: %v", err)
	}
	root, target, native := s.cfg.Root, s.cfg.RemoteTarget, s.cfg.RemoteNative
	if s.cfg.RemoteBinding {
		b, err := s.turnBinding(eventID)
		if err != nil {
			r := &remoteTurn{s: s, sessionID: sessionID, emit: emit, turn: turn, meta: remoteMeta{State: "not_connected", Reason: err.Error()}}
			text := "Not connected. Run /amq-remote in a Claude Code or Codex session."
			if !errors.Is(err, binding.ErrNone) {
				text = "Not connected: " + err.Error()
			}
			return r.say(r.settle("replied"), StopReasonRefusal, text)
		}
		root, target, native = b.Root, b.Target, b.NativeSession
	}
	r := &remoteTurn{
		s:         s,
		dir:       filepath.Join(root, remoteStateDir),
		target:    target,
		native:    native,
		sessionID: sessionID,
		emit:      emit,
		turn:      turn,
		// The exact reference exists before any IPC, so an unknown submit
		// outcome still names the request (codex #876 P1 #2).
		meta: remoteMeta{Target: target, RequestRef: protocol.EncodeRef(ipc.LocalHost, target, id)},
	}

	// A redelivered event follows its stored request, whatever the target's
	// epoch is now (codex #876 P2 #4). A busy-rejected record is resubmitted
	// under its stored epoch, so the endpoint's own retry rule decides it
	// (codex #876 r2 P2 #4). A failed lookup proves nothing about absence
	// (codex #876 r2 P1 #2).
	epoch := ""
	if eventID != "" {
		rep, err := r.get()
		switch {
		case err == nil && rep.Snapshot.State == protocol.StateRejected && rep.Snapshot.Code == protocol.CodeBusy:
			epoch = rep.Snapshot.Epoch
		case err == nil:
			return r.follow(rep.Snapshot)
		case !hasCode(err, protocol.CodeNotFound):
			return r.failed(remoteUncertain, err)
		}
	}
	if epoch == "" {
		session, err := remoteSession(r.dir, r.target, r.native)
		if err != nil {
			return r.failed(remoteNotSubmitted, err)
		}
		epoch = session.Epoch
	}
	rep, err := r.call(&protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: id,
		TargetID:  r.target,
		Epoch:     epoch,
		NotAfter:  protocol.FormatTime(time.Now().Add(remoteAdmitWithin)),
		Input:     &protocol.SubmitInput{Text: text, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn},
	})
	if err != nil {
		// Unreachable and unshared refuse before the endpoint sees the
		// command. Any other error may follow a native run, so the exact
		// record decides (codex #876 P1 #2).
		if hasCode(err, protocol.CodeEndpointUnreachable) || hasCode(err, protocol.CodeUnshared) {
			return r.failed(remoteNotSubmitted, err)
		}
		stored, gerr := r.get()
		switch {
		case gerr == nil:
			return r.follow(stored.Snapshot)
		case hasCode(gerr, protocol.CodeNotFound):
			return r.failed(remoteNotSubmitted, err)
		default:
			return r.failed(remoteUncertain, err)
		}
	}
	if rep.Outcome.Code != "" {
		r.meta.Code, r.meta.Reason = string(rep.Outcome.Code), rep.Outcome.Message
		r.meta.State = string(rep.Snapshot.State)
		if outcome := r.settle("replied"); outcome != "replied" {
			return r.settled(outcome, rep.Snapshot)
		}
		return r.say("replied", StopReasonRefusal, r.statusText(rep.Snapshot))
	}
	return r.follow(rep.Snapshot)
}

// turnBinding fixes the binding a prompt runs under. A redelivered event
// follows the binding its first delivery recorded, never the current one,
// so a rebind between deliveries cannot run the event twice in two sessions
// (codex #885 P1 #1). A first delivery records its binding before submit.
func (s *Server) turnBinding(eventID string) (binding.Binding, error) {
	if eventID == "" {
		return binding.Read()
	}
	path := filepath.Join(s.cfg.StateDir, "remote-events", eventID+".json")
	if raw, err := readSmallRegular(path); err == nil {
		var b binding.Binding
		if err := json.Unmarshal(raw, &b); err != nil || b.Target == "" || b.NativeSession == "" || !filepath.IsAbs(b.Root) {
			return binding.Binding{}, fmt.Errorf("event %s has an unreadable recorded binding; refusing to resubmit", eventID)
		}
		// A claim can be visible before its directory entry is durable. Every
		// reader makes it durable before it submits, so a crash cannot lose
		// the claim behind a submission (codex #885 r3 P1).
		if err := fsq.SyncDir(filepath.Dir(path)); err != nil {
			return binding.Binding{}, err
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return binding.Binding{}, err
	}
	b, err := binding.Read()
	if err != nil {
		return b, err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return binding.Binding{}, err
	}
	// The first delivery claims the event exclusively. A concurrent first
	// delivery that loses reads the winner's binding and never uses the one
	// it captured, so all deliveries of one event share one session
	// (codex #885 r2 P1).
	won, err := createExclusive(path, raw)
	if err != nil {
		return binding.Binding{}, err
	}
	if !won {
		return s.turnBinding(eventID)
	}
	return b, nil
}

// createExclusive publishes raw at path only if nothing is there yet. The
// bytes are complete and synced before the link makes them visible, so a
// reader never sees a partial claim. It reports whether this call won.
func createExclusive(path string, raw []byte) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".claim-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, fsq.SyncDir(dir)
}

// readSmallRegular reads a small regular file, refusing a symlink leaf.
func readSmallRegular(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 64*1024 {
		return nil, fmt.Errorf("%s is not a small regular file; refusing", path)
	}
	return os.ReadFile(path)
}

// follow waits on the endpoint until the request is terminal or uncertain,
// the client cancels or leaves, or the turn times out.
func (r *remoteTurn) follow(snap protocol.Snapshot) (any, *rpcError) {
	if snap.State.Terminal() || snap.State == protocol.StateUncertain {
		return r.settled(r.settle("replied"), snap)
	}
	if err := emitText(r.emit, r.sessionID, "agent_thought_chunk", fmt.Sprintf("Submitted to %s as %s.", r.meta.Target, snap.RequestRef)); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP session update: %v", err)
	}

	// The goroutine reads only this immutable reference; snap belongs to the
	// consumer loop below (codex #876 r2 P1 #1).
	ref, native, timeout := snap.RequestRef, r.native, r.s.cfg.HeartbeatInterval.Milliseconds()
	waits := make(chan remoteWait, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			resp, err := ipc.Call(r.dir, ipc.Request{Wait: &ipc.WaitRequest{RequestRef: ref, TimeoutMS: timeout}, NativeSession: native})
			select {
			case waits <- remoteWait{resp, err}:
			case <-stop:
				return
			}
			if err != nil || resp.Error != nil || !resp.TimedOut {
				return
			}
		}
	}()

	deadline := time.NewTimer(r.s.cfg.TurnTimeout)
	defer deadline.Stop()
	for {
		select {
		case <-r.turn.done:
			return r.settled(r.settle(""), snap)
		case <-deadline.C:
			return r.settled(r.settle("reply_timeout"), snap)
		case w := <-waits:
			if w.err == nil {
				w.err = w.resp.AsError()
			}
			if w.err != nil {
				return r.failed(remoteUncertain, w.err)
			}
			var current protocol.Snapshot
			if err := json.Unmarshal(w.resp.Reply, &current); err != nil {
				return nil, newRPCError(codeInternalError, "decode request snapshot: %v", err)
			}
			snap = current
			if w.resp.TimedOut {
				if err := emitText(r.emit, r.sessionID, "agent_thought_chunk", fmt.Sprintf("Still running on %s.", r.meta.Target)); err != nil {
					return nil, newRPCError(codeInternalError, "emit ACP heartbeat: %v", err)
				}
				continue
			}
			return r.settled(r.settle("replied"), snap)
		}
	}
}

// settle decides the turn's outcome if it is still open and returns the
// outcome that stands. An empty outcome only reads it.
func (r *remoteTurn) settle(outcome string) string {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if outcome != "" {
		r.turn.settleLocked(outcome)
	}
	return r.turn.outcome
}

// settled renders the turn once its outcome stands. The first settled
// outcome wins on every path (codex #876 P2 #5): a cancel that won is
// reported even when the request later completed.
func (r *remoteTurn) settled(outcome string, snap protocol.Snapshot) (any, *rpcError) {
	r.meta.State, r.meta.Code = string(snap.State), string(snap.Code)
	switch outcome {
	case "session_cancelled":
		return r.cancel(snap)
	case "client_disconnected":
		r.meta.Reason = outcome
		return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: r.meta}}, nil
	case "reply_timeout":
		r.meta.Reason = outcome
		return r.say(outcome, StopReasonRefusal, fmt.Sprintf("%s: request %s is still %s and the work continues. Check it with `amq-remote status %s`.", r.meta.Target, snap.RequestRef, snap.State, snap.RequestRef))
	}
	switch snap.State {
	case protocol.StateCompleted:
		text := ""
		if snap.Result != nil {
			text, r.meta.Truncated = snap.Result.Text, snap.Result.Truncated
		}
		return r.say(outcome, StopReasonEndTurn, text)
	case protocol.StateCancelled:
		return r.say(outcome, StopReasonCancelled, r.statusText(snap))
	default: // failed, rejected, uncertain
		return r.say(outcome, StopReasonRefusal, r.statusText(snap))
	}
}

// cancel asks the endpoint to cancel the exact request under the epoch it was
// stored with. The disposition comes from the reply's outcome or, for the
// usual path, its snapshot (codex #876 P1 #1). Work that keeps running is
// said so.
func (r *remoteTurn) cancel(snap protocol.Snapshot) (any, *rpcError) {
	r.meta.Reason = "session_cancelled"
	if snap.State.Terminal() {
		return remotePromptResult{StopReason: StopReasonCancelled, Meta: remotePromptMeta{Remote: r.meta}}, nil
	}
	rep, err := r.call(&protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestCancel,
		RequestRef: snap.RequestRef,
		TargetID:   snap.TargetID,
		Epoch:      snap.Epoch,
		NotAfter:   protocol.FormatTime(time.Now().Add(remoteAdmitWithin)),
	})
	switch {
	case err != nil:
		r.meta.Cancel = codeOf(err)
	case rep.Outcome.Code != "":
		r.meta.Cancel = string(rep.Outcome.Code)
	default:
		disposition := rep.Outcome.Disposition
		if disposition == "" && rep.Snapshot.Cancel != nil {
			disposition = rep.Snapshot.Cancel.Disposition
		}
		r.meta.Cancel = string(disposition)
	}
	if err == nil && rep.Snapshot.State != "" {
		r.meta.State = string(rep.Snapshot.State)
	}
	if r.meta.State == string(protocol.StateCancelled) {
		return remotePromptResult{StopReason: StopReasonCancelled, Meta: remotePromptMeta{Remote: r.meta}}, nil
	}
	return r.say("session_cancelled", StopReasonCancelled, fmt.Sprintf("%s: cancel %s; request %s is still %s and the work continues.", r.meta.Target, r.meta.Cancel, snap.RequestRef, r.meta.State))
}

// failed ends a turn whose request is absent (not_submitted) or unknown
// (uncertain). The exact reference stays in the result, so an unknown
// outcome is never an invitation to submit again.
func (r *remoteTurn) failed(state string, err error) (any, *rpcError) {
	r.meta.State, r.meta.Code, r.meta.Reason = state, codeOf(err), err.Error()
	var refusal *protocol.Refusal
	if errors.As(err, &refusal) {
		r.meta.Reason = refusal.Message
	}
	if state == remoteNotSubmitted {
		r.meta.RequestRef = ""
	}
	outcome := r.settle("replied")
	if outcome == "client_disconnected" {
		r.meta.Reason = outcome
		return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: r.meta}}, nil
	}
	stopReason := StopReasonRefusal
	if outcome == "session_cancelled" {
		stopReason = StopReasonCancelled
	}
	text := fmt.Sprintf("%s: not submitted (%s): %s", r.meta.Target, r.meta.Code, r.meta.Reason)
	if state == remoteUncertain {
		text = fmt.Sprintf("%s: the outcome of request %s is unknown: %s. Do not resend; check it with `amq-remote status %s`.", r.meta.Target, r.meta.RequestRef, r.meta.Reason, r.meta.RequestRef)
	}
	return r.say(outcome, stopReason, text)
}

// say emits text as the agent message and returns the result. A client that
// left gets no message.
func (r *remoteTurn) say(outcome, stopReason, text string) (any, *rpcError) {
	if outcome != "client_disconnected" && text != "" {
		if err := emitText(r.emit, r.sessionID, "agent_message_chunk", text); err != nil {
			return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
		}
	}
	return remotePromptResult{StopReason: stopReason, Meta: remotePromptMeta{Remote: r.meta}}, nil
}

func (r *remoteTurn) statusText(snap protocol.Snapshot) string {
	text := fmt.Sprintf("%s: request %s", r.meta.Target, r.meta.State)
	if r.meta.Code != "" {
		text += " (" + r.meta.Code + ")"
	}
	if r.meta.Reason != "" {
		text += ": " + r.meta.Reason
	} else if snap.Result != nil && snap.Result.Error != "" {
		text += ": " + snap.Result.Error
	}
	return text
}

func (r *remoteTurn) get() (protocol.Reply, error) {
	return r.call(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: r.meta.RequestRef})
}

func (r *remoteTurn) call(cmd *protocol.Command) (protocol.Reply, error) {
	resp, err := ipc.Call(r.dir, ipc.Request{Command: cmd, NativeSession: r.native})
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

// remoteRequestID is the endpoint request id for one prompt. A Nostr event id
// maps to a fixed id, so a redelivered event reconciles with its first submit
// instead of running twice.
func remoteRequestID(eventID string) (string, error) {
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
