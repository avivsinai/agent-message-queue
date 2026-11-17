package acp

import (
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// mailboxSteerTarget names a published mailbox turn: the bound handle in
// the binding's root, the thread, and the prompt message that turn
// published. event is the turn's Buzz event.
type mailboxSteerTarget struct {
	binding binding.Binding
	thread  string
	prompt  string
	event   string
}

// steerClaim is the durable record of one steer event,
// <steer-event>.steer.json in the shared state dir: the message id, time and
// destination of the steer, and the event of the turn it steered.
type steerClaim struct {
	MessageID   string `json:"message_id"`
	Created     string `json:"created"`
	Binding     string `json:"binding,omitempty"`
	Root        string `json:"root"`
	Handle      string `json:"handle"`
	Thread      string `json:"thread"`
	Prompt      string `json:"prompt"`
	ParentEvent string `json:"parent_event,omitempty"`
}

// openMailboxSteering records the target of a published mailbox turn, once
// its prompt is in the handle's inbox.
func (s *Server) openMailboxSteering(turn *turnState, b binding.Binding, thread, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turn.mailboxSteer = &mailboxSteerTarget{binding: b, thread: thread, prompt: prompt, event: turn.mailboxEvent}
}
