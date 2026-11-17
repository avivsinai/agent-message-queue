package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/wakecmd"
)

// ownerCommandAge bounds how long ago the owner may have signed a command
// that is still typed.
const ownerCommandAge = 5 * time.Minute

// The waits for the wake's answer to a command. The wake reads its queue
// every 2 s, so a live wake answers within commandAnswerWait; a queued
// command is followed for commandTypedWait. Tests shorten them.
var (
	commandAnswerWait = 6 * time.Second
	commandTypedWait  = time.Hour
	commandPoll       = 500 * time.Millisecond
)

// ownerCommand is a slash command the owner typed in Buzz: queued for the
// seat's wake (agentDir, id), or refused with one line.
type ownerCommand struct {
	text     string
	handle   string
	agentDir string
	id       string
	refusal  string
}

// ownerCommandOf reads the owner's message m whose verified content is one
// line that starts with "/", and queues it for the wake of the channel's
// mailbox seat, which types it into the terminal like Remote Control. ok is
// false for any other message: that goes to the session as an ordinary
// message.
func (s *Server) ownerCommandOf(m ownerMsg) (cmd ownerCommand, ok bool) {
	channel := m.channel
	if channel == "" || s.id.Owner == "" || !strings.HasPrefix(m.words, "/") {
		return cmd, false
	}
	if !s.cfg.Machine {
		return cmd, false
	}
	r, found := s.channelRoute(channel)
	if !found {
		return cmd, false
	}
	// The channel commands only the session it was made for, never a
	// later binding of the same name.
	name := r.Name
	b, err := binding.ReadNamed(name)
	if err != nil || !s.serves(r, b) {
		return cmd, false
	}
	evt := m.event
	cmd.text = dropMentions(evt.Content)
	if !strings.HasPrefix(cmd.text, "/") {
		return cmd, false
	}
	now := time.Now()
	switch {
	case !signedWithin(evt, now.Add(-ownerCommandAge), now):
		cmd.refusal = "Not typed: the command was sent more than 5 minutes ago."
	case !b.Mailbox():
		cmd.refusal = "Not typed: a native share has no terminal to type into; share the session with /amq-remote instead."
	case b.Harness != harness.Claude:
		running := b.Harness
		if running == "" {
			running = "an unknown harness"
		}
		cmd.refusal = "Not typed: only a Claude Code session takes commands from Buzz, and this one runs " + running + "."
	default:
		if err := wakecmd.Check(cmd.text); err != nil {
			cmd.refusal = "Not typed: " + err.Error() + "."
			break
		}
		cmd.handle, cmd.agentDir = b.Handle, fsq.AgentBase(b.Root, b.Handle)
		cmd.id, err = enqueueCommand(s.id.Owner, name, channel, cmd, b, evt, now)
		switch {
		case errors.Is(err, wakecmd.ErrDuplicate):
			cmd.refusal = "Already taken."
		case err != nil:
			cmd.refusal = "Not typed: " + err.Error() + "."
		}
	}
	return cmd, true
}

// enqueueCommand queues cmd for the wake of b's seat with the signed event
// evt, after it records the owner and channel that the wake verifies evt
// against.
func enqueueCommand(owner, name, channel string, cmd ownerCommand, b binding.Binding, evt nostr.Event, now time.Time) (string, error) {
	raw, err := json.Marshal(evt)
	if err != nil {
		return "", err
	}
	dir, err := dialog.DirFor(name)
	if err != nil {
		return "", err
	}
	if err := dialog.WriteOwner(dir, dialog.Owner{PubKey: owner, Channel: channel, Target: b.SessionIdentity()}); err != nil {
		return "", err
	}
	return wakecmd.Enqueue(cmd.agentDir, evt.ID.Hex(), wakecmd.Request{Target: b.SessionIdentity(), Text: cmd.text, Event: raw}, now)
}

// commandReply waits for the wake's first answer to cmd and returns the
// line for the owner. A queued command is followed, and the owner is told
// again once the wake types or refuses it. "Typed" is all the wake can
// know: it never says the command ran.
func (s *Server) commandReply(channel string, cmd ownerCommand) string {
	if cmd.refusal != "" {
		return cmd.refusal
	}
	state, reason, ok := s.awaitCommand(cmd, commandAnswerWait, "")
	if !ok {
		if wakecmd.Withdraw(cmd.agentDir, cmd.id) {
			return fmt.Sprintf("Not typed: no live wake for %s took the command.", cmd.handle)
		}
		// The wake claimed it as the wait ended; its answer follows.
		if state, reason, ok = s.awaitCommand(cmd, commandAnswerWait, ""); !ok {
			return "Not confirmed: the wake took " + cmd.text + " but did not answer."
		}
	}
	if state == wakecmd.Queued {
		go func() {
			if state, reason, ok := s.awaitCommand(cmd, commandTypedWait, wakecmd.Queued); ok {
				s.publish(channel, commandLine(cmd.text, state, reason))
			}
		}()
	}
	return commandLine(cmd.text, state, reason)
}

// commandLine is the owner's line for one wake answer.
func commandLine(text, state, reason string) string {
	switch state {
	case wakecmd.Typed:
		return "Typed " + text + " into the terminal."
	case wakecmd.Queued:
		return "Queued " + text + ": " + reason + ". It is typed once the terminal is free."
	}
	return "Not typed: " + reason + "."
}

// awaitCommand polls cmd's outcome for up to wait and returns the first
// one whose state is not skip.
func (s *Server) awaitCommand(cmd ownerCommand, wait time.Duration, skip string) (state, reason string, ok bool) {
	deadline := time.Now().Add(wait)
	for {
		if state, reason, ok := wakecmd.Outcome(cmd.agentDir, cmd.id); ok && state != skip {
			return state, reason, true
		}
		if !time.Now().Before(deadline) || s.stopped() {
			return "", "", false
		}
		time.Sleep(commandPoll)
	}
}
