package core

import (
	"slices"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Targets returns the registered target ids.
func (e *Endpoint) Targets() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.targets))
	for id := range e.targets {
		out = append(out, id)
	}
	return out
}

// Store exposes the record store for carrier-side recovery reads:
// the AMQ carrier reconciles claimed-but-unreceipted commands by reading the
// durable record instead of re-executing the command. Read-only use.
func (e *Endpoint) Store() *requests.Store { return e.store }

// Sessions is the attached-target projection session.list returns, for the
// serving process to print after attachment.
func (e *Endpoint) Sessions() []protocol.Session { return e.list() }

func (e *Endpoint) list() []protocol.Session {
	e.mu.Lock()
	ids := make([]string, 0, len(e.targets))
	for id := range e.targets {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	out := make([]protocol.Session, 0, len(ids))
	for _, id := range ids {
		// sessionProjection masks the steer capability the v1 gate
		// refuses so the advertised capabilities match what the endpoint
		// actually accepts.
		s, err := e.sessionProjection(id)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

// sessionProjection returns the attachment's Inspect() result with
// capabilities masked to match what the endpoint actually accepts. The
// v1 gate refuses deliver=steer, so advertising Steer is a
// false capability signal — a client that picks operations from the
// advertised capabilities is told it can steer, then every steer is
// refused. This helper is used by BOTH list() and inspect() so the mask is
// in one place.
func (e *Endpoint) sessionProjection(targetID string) (protocol.Session, error) {
	e.mu.Lock()
	t, ok := e.targets[targetID]
	e.mu.Unlock()
	if !ok {
		return protocol.Session{}, protocol.Refuse(protocol.CodeNotFound, "target %s is not registered", targetID)
	}
	s := t.att.Inspect()
	// Mask steer while the v1 gate refuses deliver=steer.
	s.Capabilities.Steer = false
	return s, nil
}

// NativeIdentifier is implemented by an attachment that can prove the
// harness's own identity for its attached session (Codex thread id, Claude
// session id). It is in-process only, never on the session wire schema.
type NativeIdentifier interface {
	NativeSessionID() string
}

// AttachmentOf returns a target's attachment for in-process seams such as
// activity export, which assert the narrow interface they need.
func (e *Endpoint) AttachmentOf(targetID string) (Attachment, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.targets[targetID]
	if !ok {
		return nil, false
	}
	return t.att, true
}

// NativeSessionID returns the attached native session identity of a target,
// or "" when the target is unknown or its adapter cannot prove one.
func (e *Endpoint) NativeSessionID(targetID string) string {
	e.mu.Lock()
	t, ok := e.targets[targetID]
	e.mu.Unlock()
	if !ok {
		return ""
	}
	if n, ok := t.att.(NativeIdentifier); ok {
		return n.NativeSessionID()
	}
	return ""
}

func (e *Endpoint) inspect(targetID string) (any, error) {
	return e.sessionProjection(targetID)
}

// listFor is session.list as src may see it: every target for an unfiltered
// source, only the shared ones for a link (see linkView).
func (e *Endpoint) listFor(src Source) []protocol.Session {
	all := e.list()
	if src.Shared == nil {
		return all
	}
	out := make([]protocol.Session, 0, len(src.Shared))
	for _, s := range all {
		if slices.Contains(src.Shared, s.TargetID) {
			out = append(out, linkView(s, src))
		}
	}
	return out
}

// inspectFor is session.inspect as src may see it. A target not shared with
// a link is not_found, the same as an unregistered one.
func (e *Endpoint) inspectFor(targetID string, src Source) (any, error) {
	if src.Shared == nil {
		return e.inspect(targetID)
	}
	if !slices.Contains(src.Shared, targetID) {
		return nil, protocol.Refuse(protocol.CodeNotFound, "target %s is not registered", targetID)
	}
	s, err := e.sessionProjection(targetID)
	if err != nil {
		return nil, err
	}
	return linkView(s, src), nil
}

// linkView removes from a session what a link may not see: the active request
// and pending interaction unless the link created that request, and the
// answering capabilities, since no link answers a tool prompt.
func linkView(s protocol.Session, src Source) protocol.Session {
	own := false
	if s.ActiveRequestRef != nil {
		host, _, _, err := protocol.DecodeRef(*s.ActiveRequestRef)
		own = err == nil && host == src.Host
	}
	if !own {
		s.ActiveRequestRef = nil
		s.PendingInteraction = nil
	}
	s.Capabilities.ApproveTool = false
	s.Capabilities.AnswerQuestion = false
	return s
}
