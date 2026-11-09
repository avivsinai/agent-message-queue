package acp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
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

// ownerCommandFor reads the owner's message eventID in channel whose
// verified content (ownerEvent) is one line that starts with "/", and
// queues it for the wake of the channel's mailbox seat, which types it into
// the terminal like Remote Control. ok is false for any other message,
// including a "/" line that is not verified as the owner's: that goes to
// the session as an ordinary message.
func (s *Server) ownerCommandFor(channel, eventID, prompt string) (cmd ownerCommand, ok bool) {
	if channel == "" || buzzOwner == "" || !strings.HasPrefix(ownerWords(prompt), "/") {
		return cmd, false
	}
	name := s.cfg.Pin
	if s.cfg.Machine {
		r, found := s.channelRoute(channel)
		if !found {
			return cmd, false
		}
		name = r.Name
	} else if s.cfg.Helper || name == "" || s.cfg.PinErr != nil {
		return cmd, false
	}
	b, err := binding.ReadNamed(name)
	if err != nil || !s.live(b) {
		return cmd, false
	}
	evt, err := ownerEvent(eventID, prompt, channel)
	if err != nil {
		return cmd, false
	}
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
		cmd.id, err = wakecmd.Enqueue(cmd.agentDir, evt.ID.Hex(), cmd.text, now)
		switch {
		case errors.Is(err, wakecmd.ErrDuplicate):
			cmd.refusal = "Already taken."
		case err != nil:
			cmd.refusal = "Not typed: " + err.Error() + "."
		}
	}
	return cmd, true
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

// answerCommand tells the owner what became of a command and ends the
// prompt without delivering it to the session as a message.
func (s *Server) answerCommand(sessionID, channel string, cmd ownerCommand, emit func(any) error) (any, *rpcError) {
	text := s.commandReply(channel, cmd)
	meta := remoteMeta{State: "answered", Posted: s.publish(channel, text)}
	if err := emitText(emit, sessionID, "agent_message_chunk", text); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
	}
	return remotePromptResult{StopReason: StopReasonEndTurn, Meta: remotePromptMeta{Remote: meta}}, nil
}
