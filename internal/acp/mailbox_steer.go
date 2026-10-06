package acp

import (
	"encoding/json"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// mailboxSteerTarget is where a follow-up DM goes while a mailbox turn is
// open: the bound handle in the binding's root, on the thread and with refs
// to the prompt message that turn published.
type mailboxSteerTarget struct {
	binding binding.Binding
	thread  string
	prompt  string
}

// openMailboxSteering makes a published mailbox turn steerable. A turn is
// steerable only after its prompt is in the handle's inbox, so a follow-up
// never reaches the handle before the prompt it refs.
func (s *Server) openMailboxSteering(turn *turnState, b binding.Binding, thread, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turn.mailboxSteer = &mailboxSteerTarget{binding: b, thread: thread, prompt: prompt}
}

// errMailboxSteering is the refusal for a steer in binding mode that has no
// open, published mailbox turn to join: an idle session, a native binding,
// or a prompt not yet delivered. It keeps the amq-remote refusal code.
func errMailboxSteering() *rpcError {
	return newRPCError(codeMethodNotFound, "steering needs an open mailbox turn in amq-remote mode")
}

// mailboxSteering delivers a follow-up DM into an open mailbox turn
// (bead agent-message-queue-cfy). It is an urgent buzz-steer message from
// buzz to the bound handle on the turn's thread that refs the turn's prompt,
// so `amq reply` to it also answers the turn. The turn keeps waiting for its
// final reply. The check and the delivery happen under s.mu, as for cockpit
// steering, so a turn that has just settled is never reported as injected.
func (s *Server) mailboxSteering(params json.RawMessage) (any, *rpcError) {
	var parsed steeringParams
	if err := decodeParams(params, &parsed, false); err != nil {
		return nil, err
	}
	text, rpcErr := steeringText(parsed.Prompt)
	if rpcErr != nil {
		return nil, rpcErr
	}
	eventID, rpcErr := resolveEventID(parsed.Meta)
	if rpcErr != nil {
		return nil, rpcErr
	}
	s.mu.Lock()
	session, ok := s.sessions[parsed.SessionID]
	if !ok {
		s.mu.Unlock()
		return nil, newRPCError(codeInvalidParams, "unknown sessionId %q", parsed.SessionID)
	}
	turn := session.turn
	if turn == nil || turn.outcome != "" || turn.mailboxSteer == nil {
		s.mu.Unlock()
		return nil, errMailboxSteering()
	}
	target := *turn.mailboxSteer
	cfg := s.cfg
	cfg.Root, cfg.Me, cfg.To = target.binding.Root, mailboxSender, target.binding.Handle
	body := formatSteeringBody(text) + "\n\nThis follow-up came from the owner's Buzz DM. Keep answering that DM with `amq reply --id " + target.prompt + "`."
	delivery, err := DeliverSteering(cfg, body, target.thread, target.prompt, eventID)
	s.mu.Unlock()
	if err != nil {
		return nil, newRPCError(codeInternalError, "deliver steering to %s: %v", target.binding.Handle, err)
	}
	outcome := SteeringInjected
	if delivery.Duplicate {
		outcome = SteeringDuplicate
	}
	return newSteeringResult(outcome, delivery), nil
}
