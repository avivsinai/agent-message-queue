package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// agentSession is the protocol between the host's loop and its agent. The
// loop owns the inbox, the journal, the reply, timeouts and cancels; a
// session owns the wire. The answer text and the refused tool calls of the
// turn in flight go into Host.turn.
type agentSession interface {
	// open starts the agent's session. recorded is the session this host
	// recorded for the same agent and cwd, or nil; the session resumes it
	// when the agent can. It returns the session id and whether recorded
	// was resumed.
	open(ctx context.Context, recorded *sessionRecord) (id string, resumed bool, err error)
	// agentName names the agent ("name version") once open returned.
	agentName() string
	// send starts one turn with text. sent runs before any byte of the turn
	// is written; its error, or a failure before it, is returned and nothing
	// reached the agent. The channel yields the turn's end exactly once.
	send(text string, meta map[string]any, sent func(id int64) error) (<-chan turnEnd, error)
	// cancel asks the agent to stop the turn in flight. The channel closes
	// once the request is delivered or has failed, and not before: a cancel
	// still on its way could reach a later turn.
	cancel() <-chan struct{}
	// done closes when the agent is gone; err says why.
	done() <-chan struct{}
	err() error
}

// turnEnd is how the agent ended one turn: an ACP stop reason (end_turn,
// cancelled, refusal, max_tokens, ...), or err when the turn failed. An
// *rpcError is the agent's own failure; errClosed means the agent is gone.
type turnEnd struct {
	stop string
	err  error
}

// acpSession speaks ACP version 1 over newline-delimited JSON-RPC.
type acpSession struct {
	h    *Host
	conn *conn
	name string
	// id is the opened session, for session/prompt and session/cancel.
	id string
}

func newACPSession(h *Host, in io.Reader, out io.Writer) *acpSession {
	s := &acpSession{h: h}
	s.conn = newConn(in, out, h.onRequest, h.onNotify)
	return s
}

func (s *acpSession) agentName() string     { return s.name }
func (s *acpSession) done() <-chan struct{} { return s.conn.done }
func (s *acpSession) err() error            { return s.conn.closedErr() }

func (s *acpSession) open(ctx context.Context, recorded *sessionRecord) (string, bool, error) {
	init, err := initialize(ctx, s.conn, s.h.cfg.Version)
	if err != nil {
		return "", false, err
	}
	s.name = strings.TrimSpace(init.AgentInfo.Name + " " + init.AgentInfo.Version)
	log := s.h.cfg.Log
	if recorded != nil {
		if !init.loadSession() {
			_, _ = fmt.Fprintf(log, "amq-acp host: the agent cannot load sessions; starting a new ACP session in place of %s\n", recorded.SessionID)
		} else {
			// The agent replays the session's history as session/update
			// notifications before it answers. No turn is in flight, so
			// onNotify collects none of it.
			_, err := s.conn.call(ctx, "session/load", map[string]any{"sessionId": recorded.SessionID, "cwd": s.h.cfg.Cwd, "mcpServers": []any{}}, nil)
			if err == nil {
				s.id = recorded.SessionID
				return s.id, true, nil
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return "", false, actionRequired("the agent did not answer session/load within %s", handshakeTimeout)
			}
			if authRequired(err) {
				return "", false, actionRequired("the agent needs a login: authenticate the harness once with its own CLI, then start the host again (%v)", err)
			}
			_, _ = fmt.Fprintf(log, "amq-acp host: the agent could not load session %s (%v); starting a new ACP session\n", recorded.SessionID, err)
		}
	}
	raw, err := s.conn.call(ctx, "session/new", map[string]any{"cwd": s.h.cfg.Cwd, "mcpServers": []any{}}, nil)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", false, actionRequired("the agent did not answer session/new within %s", handshakeTimeout)
		}
		if authRequired(err) {
			return "", false, actionRequired("the agent needs a login: authenticate the harness once with its own CLI, then start the host again (%v)", err)
		}
		return "", false, fmt.Errorf("open an ACP session: %w", err)
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &session); err != nil || session.SessionID == "" {
		return "", false, fmt.Errorf("open an ACP session: the agent returned no session id")
	}
	s.id = session.SessionID
	return s.id, false, nil
}

func (s *acpSession) send(text string, meta map[string]any, sent func(id int64) error) (<-chan turnEnd, error) {
	params := map[string]any{
		"sessionId": s.id,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
	}
	if meta != nil {
		params["_meta"] = meta
	}
	dispatched := make(chan error, 1)
	ended := make(chan turnEnd, 1)
	go func() {
		ran := false
		raw, err := s.conn.call(context.Background(), "session/prompt", params, func(id int64) error {
			ran = true
			err := sent(id)
			dispatched <- err
			return err
		})
		if !ran {
			// The call ended before the hook ran.
			dispatched <- err
		}
		var res struct {
			StopReason string `json:"stopReason"`
		}
		if err == nil {
			_ = json.Unmarshal(raw, &res)
		}
		ended <- turnEnd{stop: res.StopReason, err: err}
	}()
	if err := <-dispatched; err != nil {
		return nil, err
	}
	return ended, nil
}

// cancel writes session/cancel apart from the turn, so an agent that stopped
// reading its stdin cannot hold the turn past the host's grace.
func (s *acpSession) cancel() <-chan struct{} {
	wrote := make(chan struct{})
	go func() {
		_ = s.conn.notify("session/cancel", map[string]string{"sessionId": s.id})
		close(wrote)
	}()
	return wrote
}
