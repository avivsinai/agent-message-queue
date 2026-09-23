package acp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

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
// recorded state; Cancel is the endpoint's cancel disposition or refusal code,
// set only when the client cancelled the turn.
type remoteMeta struct {
	Target     string `json:"target"`
	RequestRef string `json:"requestRef,omitempty"`
	State      string `json:"state,omitempty"`
	Code       string `json:"code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Cancel     string `json:"cancel,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type remoteWait struct {
	resp *ipc.Response
	err  error
}

// runRemote submits the prompt to the pinned amq-remote target and holds the
// turn open until the request reaches a terminal or uncertain state. A cancel
// asks the endpoint to cancel that exact request; the endpoint's answer is
// reported, so an adapter that cannot cancel never reads as cancelled work.
func (s *Server) runRemote(sessionID, text, eventID string, turn *turnState, emit func(any) error) (any, *rpcError) {
	dir := filepath.Join(s.cfg.Root, remoteStateDir)
	meta := remoteMeta{Target: s.cfg.RemoteTarget}

	session, err := remoteSession(dir, s.cfg.RemoteTarget)
	if err != nil {
		return s.remoteRefusal(sessionID, emit, meta, err)
	}
	id, err := remoteRequestID(eventID)
	if err != nil {
		return nil, newRPCError(codeInternalError, "request id: %v", err)
	}
	rep, err := remoteCall(dir, &protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: id,
		TargetID:  s.cfg.RemoteTarget,
		Epoch:     session.Epoch,
		NotAfter:  protocol.FormatTime(time.Now().Add(remoteAdmitWithin)),
		Input:     &protocol.SubmitInput{Text: text, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn},
	})
	if err != nil {
		return s.remoteRefusal(sessionID, emit, meta, err)
	}
	snap := rep.Snapshot
	meta.RequestRef, meta.State = snap.RequestRef, string(snap.State)
	if rep.Outcome.Code != "" {
		meta.Code, meta.Reason = string(rep.Outcome.Code), rep.Outcome.Message
		return s.remoteFinish(sessionID, emit, meta, snap, StopReasonRefusal)
	}
	if snap.State.Terminal() || snap.State == protocol.StateUncertain {
		return s.remoteSettled(sessionID, emit, meta, snap, turn)
	}
	if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Submitted to %s as %s.", s.cfg.RemoteTarget, snap.RequestRef)); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP session update: %v", err)
	}

	waits := make(chan remoteWait, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			resp, err := ipc.Call(dir, ipc.Request{Wait: &ipc.WaitRequest{RequestRef: snap.RequestRef, TimeoutMS: s.cfg.HeartbeatInterval.Milliseconds()}})
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

	deadline := time.NewTimer(s.cfg.TurnTimeout)
	defer deadline.Stop()
	for {
		select {
		case <-turn.done:
			s.mu.Lock()
			outcome := turn.outcome
			s.mu.Unlock()
			meta.Reason = outcome
			if outcome != "session_cancelled" {
				// The client is gone. The native work keeps running.
				return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: meta}}, nil
			}
			meta.Cancel = remoteCancel(dir, snap)
			return remotePromptResult{StopReason: StopReasonCancelled, Meta: remotePromptMeta{Remote: meta}}, nil
		case <-deadline.C:
			if outcome := s.settle(turn, "reply_timeout"); outcome != "reply_timeout" {
				continue // cancel or disconnect won; turn.done is closed
			}
			meta.Reason = "reply_timeout"
			return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: meta}}, nil
		case w := <-waits:
			if w.err == nil {
				w.err = w.resp.AsError()
			}
			if w.err != nil {
				if outcome := s.settle(turn, "replied"); outcome != "replied" {
					continue
				}
				return s.remoteRefusal(sessionID, emit, meta, w.err)
			}
			var current protocol.Snapshot
			if err := json.Unmarshal(w.resp.Reply, &current); err != nil {
				return nil, newRPCError(codeInternalError, "decode request snapshot: %v", err)
			}
			if w.resp.TimedOut {
				if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Still running on %s.", s.cfg.RemoteTarget)); err != nil {
					return nil, newRPCError(codeInternalError, "emit ACP heartbeat: %v", err)
				}
				continue
			}
			if outcome := s.settle(turn, "replied"); outcome != "replied" {
				continue
			}
			return s.remoteSettled(sessionID, emit, meta, current, turn)
		}
	}
}

// settle decides the turn's outcome if it is still open and returns the
// outcome that stands.
func (s *Server) settle(turn *turnState, outcome string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	turn.settleLocked(outcome)
	return turn.outcome
}

// remoteSettled renders a request that reached a terminal or uncertain state.
func (s *Server) remoteSettled(sessionID string, emit func(any) error, meta remoteMeta, snap protocol.Snapshot, turn *turnState) (any, *rpcError) {
	s.settle(turn, "replied")
	meta.State, meta.Code = string(snap.State), string(snap.Code)
	switch snap.State {
	case protocol.StateCompleted:
		return s.remoteFinish(sessionID, emit, meta, snap, StopReasonEndTurn)
	case protocol.StateCancelled:
		return s.remoteFinish(sessionID, emit, meta, snap, StopReasonCancelled)
	default: // failed, rejected, uncertain
		return s.remoteFinish(sessionID, emit, meta, snap, StopReasonRefusal)
	}
}

// remoteFinish emits the text the owner should read and returns the result.
// A completed request shows its native result; anything else shows the state
// and reason, so a refusal is never an empty reply.
func (s *Server) remoteFinish(sessionID string, emit func(any) error, meta remoteMeta, snap protocol.Snapshot, stopReason string) (any, *rpcError) {
	text := ""
	if snap.Result != nil {
		text = snap.Result.Text
		meta.Truncated = snap.Result.Truncated
	}
	if stopReason != StopReasonEndTurn {
		text = remoteStatusText(meta, snap)
	}
	if text != "" {
		if err := emitText(emit, sessionID, "agent_message_chunk", text); err != nil {
			return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
		}
	}
	return remotePromptResult{StopReason: stopReason, Meta: remotePromptMeta{Remote: meta}}, nil
}

func remoteStatusText(meta remoteMeta, snap protocol.Snapshot) string {
	text := fmt.Sprintf("%s: request %s", meta.Target, meta.State)
	if meta.Code != "" {
		text += " (" + meta.Code + ")"
	}
	if meta.Reason != "" {
		text += ": " + meta.Reason
	} else if snap.Result != nil && snap.Result.Error != "" {
		text += ": " + snap.Result.Error
	}
	return text
}

// remoteRefusal reports a submit or wait the endpoint refused or could not
// answer. The typed code is kept; the text names it for the owner.
func (s *Server) remoteRefusal(sessionID string, emit func(any) error, meta remoteMeta, err error) (any, *rpcError) {
	meta.Reason = err.Error()
	var refusal *protocol.Refusal
	if errors.As(err, &refusal) {
		meta.Code, meta.Reason = string(refusal.Code), refusal.Message
	}
	if meta.State == "" {
		meta.State = "not_submitted"
	}
	return s.remoteFinish(sessionID, emit, meta, protocol.Snapshot{}, StopReasonRefusal)
}

// remoteCancel asks the endpoint to cancel the exact request under the epoch
// it was stored with, and returns the disposition or the refusal code.
func remoteCancel(dir string, snap protocol.Snapshot) string {
	rep, err := remoteCall(dir, &protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestCancel,
		RequestRef: snap.RequestRef,
		TargetID:   snap.TargetID,
		Epoch:      snap.Epoch,
		NotAfter:   protocol.FormatTime(time.Now().Add(remoteAdmitWithin)),
	})
	if err != nil {
		var refusal *protocol.Refusal
		if errors.As(err, &refusal) {
			return string(refusal.Code)
		}
		return "error: " + err.Error()
	}
	if rep.Outcome.Code != "" {
		return string(rep.Outcome.Code)
	}
	return string(rep.Outcome.Disposition)
}

func remoteSession(dir, target string) (protocol.Session, error) {
	resp, err := ipc.Call(dir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: target}})
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

func remoteCall(dir string, cmd *protocol.Command) (protocol.Reply, error) {
	resp, err := ipc.Call(dir, ipc.Request{Command: cmd})
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
