package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// mailboxSender is the AMQ handle Buzz prompts come from in a mailbox
// binding. The bound agent answers it with `amq reply`.
const mailboxSender = "buzz"

// mailboxSubject labels Buzz prompts in the bound agent's inbox.
const mailboxSubject = "Buzz DM"

// mailboxClaim is the durable first-delivery record of one event in a
// mailbox binding: the message id and creation time chosen once, so a
// redelivery publishes the same message at most once and waits for the
// same reply (codex 611.36 research, replay gap).
type mailboxClaim struct {
	MessageID string `json:"message_id"`
	Created   string `json:"created"`
	// Thread is the cockpit thread of the first delivery. A redelivery on a
	// new ACP session publishes and waits there, never on its own thread
	// (codex #895 P1 #1).
	Thread string `json:"thread"`
	// Channel is the Buzz channel of the first delivery, so the late-reply
	// sweep can post a reply that arrives after the turn ended. An older
	// claim has none and is not swept.
	Channel string `json:"channel,omitempty"`
}

// mailboxOutcome is the content of an event's final-answer marker
// <event>.posted.final. The turn or the late-reply sweep creates it before
// it posts the final reply (ReplyID names that buzz inbox message), and a
// cancel creates it with Cancelled set. The exclusive create decides which
// of the two happens, once, across processes.
type mailboxOutcome struct {
	ReplyID   string `json:"reply_id,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	// NotSubmitted records that the handle's inbox refused the message after
	// the publish lock proved it absent: nothing was delivered, and a
	// redelivery of the event ends here instead of publishing it later.
	NotSubmitted bool   `json:"not_submitted,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// runMailbox delivers the prompt as an AMQ message to the bound handle and
// holds the turn until that handle answers. A reply of kind status is
// progress; any other reply that refs the prompt is the final answer. How
// the handle notices the message (a wake, a built-in consumer, monitor, or
// its next drain) is the handle owner's business, not this bridge's.
func (s *Server) runMailbox(sessionID, text, eventID string, b binding.Binding, turn *turnState, emit func(any) error) (any, *rpcError) {
	r := &remoteTurn{s: s, sessionID: sessionID, eventID: eventID, emit: emit, turn: turn, meta: remoteMeta{Target: b.Handle}}
	s.mu.Lock()
	threadID := ""
	if session, ok := s.sessions[sessionID]; ok {
		threadID = session.Thread
	}
	s.mu.Unlock()
	if threadID == "" {
		threadID = cockpitThread("session/" + sessionID)
	}
	r.mailboxThread = threadID

	// A turn cancelled before delivery delivers nothing (codex #895 P1 #3).
	if outcome := r.settle(""); outcome == "session_cancelled" || outcome == "client_disconnected" {
		return s.mailboxNotDelivered(r, outcome)
	}
	// The turn budget starts before the claim, so a publish-lock wait counts
	// against it (codex #895 r2 P1).
	budget := time.Now().Add(s.cfg.TurnTimeout)
	claim, err := s.publishClaimed(r, budget, b, threadID, text)
	if claim.MessageID != "" {
		threadID = claim.Thread
		r.mailboxThread = threadID
		r.meta.RequestRef = claim.MessageID
		// The first known channel of the event is its DM; a redelivery from
		// another channel, or from none, never moves the answer. A delivery
		// that names one records it if none is known yet (review of #961).
		s.eventChannel(eventID, claim.Channel, turn.channel)
		r.mailbox, r.claimChannel = true, claim.Channel
		s.mu.Lock()
		turn.mailboxEvent = eventID
		s.mu.Unlock()
		s.late.add(eventID)
	}
	if err != nil {
		if errors.Is(err, errStoppedBeforePublish) {
			return s.mailboxNotDelivered(r, r.settle(""))
		}
		if errors.Is(err, lock.ErrStopped) {
			if outcome := r.settle("reply_timeout"); outcome != "reply_timeout" {
				return s.mailboxNotDelivered(r, outcome)
			}
			r.meta.Reason = "reply_timeout"
			return r.say(postStatus, "reply_timeout", StopReasonRefusal, fmt.Sprintf("Not delivered to %s: the turn ran out of time before the message could be published.", b.Handle))
		}
		var refused *notDeliveredError
		if errors.As(err, &refused) {
			return s.mailboxRefused(r, b, refused.reason)
		}
		// A redelivered event that is already answered returns that answer
		// and posts nothing.
		if errors.Is(err, errEventDecided) {
			if out, ok := s.mailboxAnswered(eventID); ok {
				return s.mailboxAnsweredTurn(r, b, out)
			}
		}
		return r.failed(remoteUncertain, err)
	}
	created, err := time.Parse(time.RFC3339Nano, claim.Created)
	if err != nil {
		return nil, newRPCError(codeInternalError, "mailbox claim time: %v", err)
	}
	r.meta.State = DeliveryStateQueued
	s.openMailboxSteering(turn, b, threadID, claim.MessageID)
	if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Delivered to %s's AMQ inbox.", b.Handle)); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP session update: %v", err)
	}

	deadline := time.NewTimer(time.Until(budget))
	defer deadline.Stop()
	poll := time.NewTicker(s.cfg.PollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(s.cfg.HeartbeatInterval)
	defer heartbeat.Stop()
	read := false
	// The turn start and each heartbeat also recover replies another
	// consumer moved to cur; other polls read only new.
	watch, withCur := s.watchMailbox(b, threadID, claim.MessageID, created), true
	for {
		if !read && drained(b, claim.MessageID) {
			read = true
			r.meta.State = "read"
			if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Read by %s.", b.Handle)); err != nil {
				return nil, newRPCError(codeInternalError, "emit ACP session update: %v", err)
			}
		}
		// The sweep or another process may have claimed and posted the reply.
		if out, ok := s.mailboxAnswered(eventID); ok {
			return s.mailboxAnsweredTurn(r, b, out)
		}
		finalID, final, progress, err := watch.poll(withCur)
		withCur = false
		if err != nil {
			return nil, newRPCError(codeInternalError, "poll AMQ thread %s: %v", threadID, err)
		}
		for _, note := range progress {
			if err := emitText(emit, sessionID, "agent_thought_chunk", note); err != nil {
				return nil, newRPCError(codeInternalError, "emit ACP session update: %v", err)
			}
		}
		if final != "" {
			if outcome := r.settle("replied"); outcome != "replied" {
				return s.mailboxStopped(r, outcome, b)
			}
			r.meta.State = DeliveryStateReplied
			return s.sayReply(r, b, finalID, final)
		}
		select {
		case <-turn.done:
			return s.mailboxStopped(r, r.settle(""), b)
		case <-deadline.C:
			// Another turn or the sweep may have decided the event after this
			// loop's last check.
			if out, ok := s.mailboxAnswered(eventID); ok {
				return s.mailboxAnsweredTurn(r, b, out)
			}
			if outcome := r.settle("reply_timeout"); outcome != "reply_timeout" {
				return s.mailboxStopped(r, outcome, b)
			}
			r.meta.Reason = "reply_timeout"
			return r.say(postStatus, "reply_timeout", StopReasonRefusal, fmt.Sprintf("No final reply from %s yet. The message stays in its AMQ inbox and may still be answered.", b.Handle))
		case <-poll.C:
		case <-heartbeat.C:
			withCur = true
			if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Still waiting for a reply from %s.", b.Handle)); err != nil {
				return nil, newRPCError(codeInternalError, "emit ACP heartbeat: %v", err)
			}
		}
	}
}

// mailboxStopped ends a turn the client cancelled or left. A mailbox cannot
// recall a delivered message, so the reply says the work may still run. A
// cancel becomes the event's durable outcome unless a reply already is, and
// then the turn returns that reply instead.
func (s *Server) mailboxStopped(r *remoteTurn, outcome string, b binding.Binding) (any, *rpcError) {
	r.meta.Reason = outcome
	if outcome == "client_disconnected" {
		return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: r.meta}}, nil
	}
	if r.eventID != "" {
		final, _, err := s.decideOutcome(r.eventID, mailboxOutcome{Cancelled: true})
		if err != nil {
			return nil, newRPCError(codeInternalError, "record cancel: %v", err)
		}
		if !final.Cancelled {
			return s.answeredResult(r, b, final)
		}
	}
	return r.say(postCancel, outcome, StopReasonCancelled, fmt.Sprintf("Stopped waiting. The message stays in %s's AMQ inbox; %s may still act on it.", b.Handle, b.Handle))
}

// mailboxAnsweredTurn ends a turn whose event already has an outcome. A
// cancelled event ends as cancelled. An answered one shows the client that
// reply again and posts nothing.
func (s *Server) mailboxAnsweredTurn(r *remoteTurn, b binding.Binding, out mailboxOutcome) (any, *rpcError) {
	if out.Cancelled {
		return s.mailboxStopped(r, r.settle("session_cancelled"), b)
	}
	if outcome := r.settle("replied"); outcome != "replied" {
		return s.mailboxStopped(r, outcome, b)
	}
	return s.answeredResult(r, b, out)
}

// answeredResult shows the client the reply that is the event's outcome, or
// that its message was not delivered. It was posted by whoever decided that
// outcome; this turn posts nothing.
func (s *Server) answeredResult(r *remoteTurn, b binding.Binding, out mailboxOutcome) (any, *rpcError) {
	if out.NotSubmitted {
		r.meta.State, r.meta.Code, r.meta.Reason, r.meta.RequestRef = remoteNotSubmitted, "not_delivered", out.Reason, ""
		if err := emitText(r.emit, r.sessionID, "agent_message_chunk", notDeliveredText(b.Handle, out.Reason)); err != nil {
			return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
		}
		r.meta.Posted = "duplicate: this event already has its final outcome"
		return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: r.meta}}, nil
	}
	r.meta.State, r.meta.Reason = DeliveryStateReplied, ""
	text := fmt.Sprintf("%s already answered; its final post was already made or attempted.", b.Handle)
	if id := out.ReplyID; id != "" && id == filepath.Base(id) {
		if msg, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxCur(b.Root, mailboxSender), id+".md")); err == nil && strings.TrimSpace(msg.Body) != "" {
			text = strings.TrimSpace(msg.Body)
		}
	}
	if err := emitText(r.emit, r.sessionID, "agent_message_chunk", text); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
	}
	r.meta.Posted = "duplicate: this event already has its final outcome"
	return remotePromptResult{StopReason: StopReasonEndTurn, Meta: remotePromptMeta{Remote: r.meta}}, nil
}

// decideOutcome makes proposed the event's one durable outcome, unless
// another is already recorded, and returns the outcome that stands and
// whether proposed won. The exclusive create of <event>.posted.final is the
// single decision point for a final post (a turn's or the sweep's) and a
// cancel, across processes. .cancelled is written only for a cancelled
// outcome.
func (s *Server) decideOutcome(eventID string, proposed mailboxOutcome) (mailboxOutcome, bool, error) {
	if eventID == "" {
		return proposed, true, nil
	}
	raw, err := json.Marshal(proposed)
	if err != nil {
		return mailboxOutcome{}, false, err
	}
	won, err := s.reserveFinal(eventID, raw)
	if err != nil {
		return mailboxOutcome{}, false, err
	}
	final := proposed
	if !won {
		stored, ok := s.mailboxAnswered(eventID)
		if !ok {
			return mailboxOutcome{}, false, fmt.Errorf("event %s lost its outcome record", eventID)
		}
		final = stored
	}
	if final.Cancelled {
		path, err := s.eventCancelPath(eventID)
		if err != nil {
			return mailboxOutcome{}, false, err
		}
		if _, err := createExclusive(path, []byte("cancelled\n")); err != nil {
			return mailboxOutcome{}, false, err
		}
	}
	return final, won, nil
}

// eventChannel returns the event's DM channel: the claim's, else the one a
// later delivery recorded, else this delivery's, recorded exclusively so
// the first writer wins. "" means no delivery named a channel yet.
func (s *Server) eventChannel(eventID, claimed, turn string) string {
	if claimed != "" || eventID == "" {
		return claimed
	}
	path := filepath.Join(s.cfg.StateDir, "remote-events", eventID+".channel")
	if turn != "" {
		if _, err := createExclusive(path, []byte(turn)); err != nil {
			return ""
		}
	}
	raw, err := readSmallRegular(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// sayReply posts the final reply once per event and emits it. The event's
// DM is resolved now, from the claim or the recorded channel. If the event
// already has an outcome, or another reply or a cancel wins it now, the turn
// returns that outcome. Without a DM nothing is decided, so the sweep posts
// the reply once a delivery names a channel. A won outcome is posted before
// the ACP emission, so a failed emission never skips the post (review of
// #961).
func (s *Server) sayReply(r *remoteTurn, b binding.Binding, replyID, text string) (any, *rpcError) {
	dest := r.postChannel()
	if r.eventID != "" {
		if dest == "" {
			if out, ok := s.mailboxAnswered(r.eventID); ok {
				return s.adoptOutcome(r, b, out)
			}
		} else {
			final, won, err := s.decideOutcome(r.eventID, mailboxOutcome{ReplyID: replyID})
			switch {
			case err != nil:
				r.meta.Posted = "error: record post marker: " + err.Error()
				dest = ""
			case !won:
				return s.adoptOutcome(r, b, final)
			}
		}
	}
	if dest != "" {
		r.meta.Posted = s.publish(dest, text)
	}
	if err := emitText(r.emit, r.sessionID, "agent_message_chunk", text); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
	}
	return remotePromptResult{StopReason: StopReasonEndTurn, Meta: remotePromptMeta{Remote: r.meta}}, nil
}

// adoptOutcome ends a turn whose own reply lost the event's outcome: it
// returns the winning reply, or ends cancelled when a cancel won.
func (s *Server) adoptOutcome(r *remoteTurn, b binding.Binding, out mailboxOutcome) (any, *rpcError) {
	if out.Cancelled {
		r.meta.Reason = "session_cancelled"
		return r.say(postCancel, "session_cancelled", StopReasonCancelled, fmt.Sprintf("Stopped waiting. The message stays in %s's AMQ inbox; %s may still act on it.", b.Handle, b.Handle))
	}
	return s.answeredResult(r, b, out)
}

// mailboxAnswered returns the event's outcome once its final-answer marker
// exists. An unreadable marker counts as an outcome with no reply id.
func (s *Server) mailboxAnswered(eventID string) (mailboxOutcome, bool) {
	if eventID == "" {
		return mailboxOutcome{}, false
	}
	raw, err := readSmallRegular(filepath.Join(s.cfg.StateDir, "remote-events", eventID+".posted."+postFinal))
	if err != nil {
		return mailboxOutcome{}, !errors.Is(err, os.ErrNotExist)
	}
	var out mailboxOutcome
	_ = json.Unmarshal(raw, &out)
	return out, true
}

// mailboxClaim returns the message id and time for this prompt, and whether
// this call created the claim. With an event id it is claimed exclusively
// under the publish lock, and a redelivery reuses it.
func (s *Server) mailboxClaim(eventID, threadID, channel string) (mailboxClaim, bool, error) {
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return mailboxClaim{}, false, err
	}
	fresh := mailboxClaim{MessageID: id, Created: now.UTC().Format(time.RFC3339Nano), Thread: threadID, Channel: channel}
	if eventID == "" {
		return fresh, true, nil
	}
	path := filepath.Join(s.cfg.StateDir, "remote-events", eventID+".mailbox.json")
	raw, err := json.Marshal(fresh)
	if err != nil {
		return mailboxClaim{}, false, err
	}
	created, err := createExclusive(path, raw)
	if err != nil {
		return mailboxClaim{}, false, err
	}
	stored, err := readSmallRegular(path)
	if err != nil {
		return mailboxClaim{}, false, err
	}
	if err := fsq.SyncDir(filepath.Dir(path)); err != nil {
		return mailboxClaim{}, false, err
	}
	var claim mailboxClaim
	if err := json.Unmarshal(stored, &claim); err != nil || claim.MessageID == "" || claim.Thread == "" {
		return mailboxClaim{}, false, fmt.Errorf("event %s has an unreadable mailbox claim; refusing to publish", eventID)
	}
	return claim, created, nil
}

// errStoppedBeforePublish means the turn was cancelled or left before the
// message was published; nothing was delivered.
var errStoppedBeforePublish = errors.New("stopped before publish")

// errEventDecided means the event already had its outcome when the publish
// lock was held, so nothing was published; the turn adopts that outcome.
var errEventDecided = errors.New("event already has its outcome")

// notDeliveredError means publishOnce found the message absent from the
// handle's inbox and the inbox then refused it: nothing was delivered.
type notDeliveredError struct{ reason string }

func (e *notDeliveredError) Error() string { return e.reason }

// publishClaimed claims the event's message and publishes it under a
// per-event lock, so two retries cannot both pass the absence check and
// publish the same message twice (codex #895 P1 #2). It returns the claim
// once it was read. The lock wait gives up on cancel, disconnect or the turn
// budget, and cancellation is checked again once the lock is held, so a
// cancelled prompt is never published (codex #895 r2 P1).
func (s *Server) publishClaimed(r *remoteTurn, budget time.Time, b binding.Binding, threadID, text string) (mailboxClaim, error) {
	var claim mailboxClaim
	publish := func() error {
		// The claim is created under the lock, so only the attempt that
		// created it can know no earlier attempt published it
		// (bead agent-message-queue-qff).
		c, fresh, err := s.mailboxClaim(r.eventID, threadID, r.turn.channel)
		if err != nil {
			return err
		}
		claim = c
		created, err := time.Parse(time.RFC3339Nano, c.Created)
		if err != nil {
			return fmt.Errorf("mailbox claim time: %w", err)
		}
		// An outcome recorded before this turn held the lock stands: a
		// refused delivery is never published by a later redelivery.
		if _, ok := s.mailboxAnswered(r.eventID); ok {
			return errEventDecided
		}
		if outcome := r.settle(""); outcome == "session_cancelled" || outcome == "client_disconnected" {
			return errStoppedBeforePublish
		}
		// An expired budget publishes nothing, also when the lock was free
		// or there is no event lock at all (codex #895 r3 P2).
		if !time.Now().Before(budget) {
			return lock.ErrStopped
		}
		err = publishOnce(b, c.Thread, c.MessageID, created, text)
		var refused *notDeliveredError
		if !errors.As(err, &refused) {
			return err
		}
		// An earlier attempt may have delivered the message before the
		// mailbox went away, so a redelivery's refusal stays unknown.
		if !fresh {
			return errors.New(refused.reason)
		}
		// Record the refusal as the event's outcome under the publish lock,
		// so no redelivery can publish between the check and the record.
		_, won, decideErr := s.decideOutcome(r.eventID, mailboxOutcome{NotSubmitted: true, Reason: refused.reason})
		switch {
		case decideErr != nil:
			return decideErr
		case !won:
			return errEventDecided
		}
		return err
	}
	if r.eventID == "" {
		return claim, publish()
	}
	if !lock.AdvisoryLockAvailable() {
		return claim, errors.New("refusing to publish a Buzz event without an advisory file lock")
	}
	stopped := func() bool {
		select {
		case <-r.turn.done:
			return true
		default:
		}
		return !time.Now().Before(budget)
	}
	path := filepath.Join(s.cfg.StateDir, "remote-events", r.eventID+".mailbox.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return claim, err
	}
	err := lock.WithExclusiveFileLockUntil(path, stopped, publish)
	if errors.Is(err, lock.ErrStopped) {
		if outcome := r.settle(""); outcome == "session_cancelled" || outcome == "client_disconnected" {
			return claim, errStoppedBeforePublish
		}
	}
	return claim, err
}

// mailboxNotDelivered ends a turn stopped before its message was published:
// nothing reached the handle, and a cancel is recorded against replay.
func (s *Server) mailboxNotDelivered(r *remoteTurn, outcome string) (any, *rpcError) {
	r.meta.Reason = outcome
	if outcome == "client_disconnected" {
		return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: r.meta}}, nil
	}
	if r.eventID != "" {
		if err := s.recordEventCancel(r.eventID); err != nil {
			return nil, newRPCError(codeInternalError, "record cancel: %v", err)
		}
	}
	return remotePromptResult{StopReason: StopReasonCancelled, Meta: remotePromptMeta{Remote: r.meta}}, nil
}

// publishOnce delivers the prompt to the handle unless the claimed message
// is already in its inbox (new or cur), so a redelivery never duplicates it.
// It never creates the handle's mailbox or inbox. Once the message is known
// absent, a refusal by the handle's mailbox is a *notDeliveredError; any
// other error leaves the outcome unknown.
func publishOnce(b binding.Binding, threadID, id string, created time.Time, text string) error {
	identity, err := fsq.SnapshotDeliveryRoot(b.Root)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(b.Root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// The handle answers buzz with `amq reply`, which cannot create a missing
	// mailbox in a root without config.json, so the sender ensures its own.
	// This runs before the redelivery check: a prompt an older amq-acp
	// delivered still needs the mailbox for its reply (#951).
	if err := root.EnsureAgentDirs(mailboxSender); err != nil {
		return err
	}
	name := id + ".md"
	for _, dir := range []string{fsq.AgentInboxNew(b.Root, b.Handle), fsq.AgentInboxCur(b.Root, b.Handle)} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	body := strings.TrimRight(text, "\n") + "\n\n" +
		"This came from the owner's Buzz DM. Answer with `amq reply --id " + id + "`; your reply is shown in that DM. " +
		"A reply with `--kind status` shows as progress; any other reply is the final answer."
	message := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     mailboxSender,
			To:       []string{b.Handle},
			Thread:   threadID,
			Subject:  mailboxSubject,
			Created:  created.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityNormal,
			Labels:   []string{"acp", "buzz"},
		},
		Body: body,
	}
	data, err := message.Marshal()
	if err != nil {
		return err
	}
	if len(data) > format.MaxMessageSize {
		return fmt.Errorf("prompt exceeds the maximum AMQ message size")
	}
	if err := completeMailbox(b.Root, b.Handle); err != nil {
		return err
	}
	if _, err := fsq.DeliverToExistingInbox(root, b.Handle, name, data); err != nil {
		var uncertain *fsq.CommittedDurabilityError
		if !errors.As(err, &uncertain) {
			return &notDeliveredError{reason: "its inbox refused the message: " + err.Error()}
		}
	}
	return nil
}

// completeMailbox refuses delivery unless the handle's mailbox and inbox
// already exist as real directories (not symlinks), and creates the other
// leaves an older amq did not make, such as receipts/. Like the absence
// check it reads the bound root path; delivery then rechecks the whole
// layout through the pinned root.
func completeMailbox(rootPath, handle string) error {
	dir := filepath.Join(rootPath, "agents", handle)
	before, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &notDeliveredError{reason: "no mailbox in its AMQ root"}
	case err != nil:
		return &notDeliveredError{reason: err.Error()}
	case !before.IsDir():
		return &notDeliveredError{reason: "its mailbox is not a directory"}
	}
	mailbox, err := os.OpenRoot(dir)
	if err != nil {
		return &notDeliveredError{reason: err.Error()}
	}
	defer func() { _ = mailbox.Close() }()
	if opened, err := mailbox.Stat("."); err != nil || !os.SameFile(before, opened) {
		return &notDeliveredError{reason: "its mailbox was replaced during delivery"}
	}
	before, err = mailbox.Lstat("inbox")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &notDeliveredError{reason: "its mailbox has no inbox"}
	case err != nil:
		return &notDeliveredError{reason: err.Error()}
	case !before.IsDir():
		return &notDeliveredError{reason: "its inbox is not a directory"}
	}
	inbox, err := mailbox.OpenRoot("inbox")
	if err != nil {
		return &notDeliveredError{reason: err.Error()}
	}
	defer func() { _ = inbox.Close() }()
	if opened, err := inbox.Stat("."); err != nil || !os.SameFile(before, opened) {
		return &notDeliveredError{reason: "its inbox was replaced during delivery"}
	}
	if err := repairMailbox(mailbox, inbox, dir); err != nil {
		return &notDeliveredError{reason: err.Error()}
	}
	return nil
}

// repairMailbox creates the missing leaves of the handle mailbox at dir,
// opened as mailbox and its inbox as inbox. It never creates either one:
// leaves are made through the opened directories, so one removed meanwhile
// makes this fail instead of reappearing.
func repairMailbox(mailbox, inbox *os.Root, dir string) error {
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		in, base, path := mailbox, dir, filepath.FromSlash(string(leaf))
		if rest, ok := strings.CutPrefix(string(leaf), "inbox/"); ok {
			in, base, path = inbox, filepath.Join(dir, "inbox"), filepath.FromSlash(rest)
		}
		if _, err := in.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			continue // present, or refused by the layout check that follows
		}
		if err := in.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("complete its mailbox: %w", err)
		}
		// Make the new directory entries durable before a message lands.
		for p := filepath.Dir(path); ; p = filepath.Dir(p) {
			if err := fsq.SyncDir(filepath.Join(base, p)); err != nil {
				return fmt.Errorf("complete its mailbox: %w", err)
			}
			if p == "." {
				break
			}
		}
	}
	return nil
}

// notDeliveredText is the reply for an event whose message never reached
// the handle's inbox.
func notDeliveredText(handle, reason string) string {
	return fmt.Sprintf("Not delivered to %s: %s. Nothing reached its inbox; send it again.", handle, reason)
}

// mailboxRefused ends the turn that recorded the event's not-submitted
// outcome. That record is the at-most-once guard for the post, as for a
// final reply.
func (s *Server) mailboxRefused(r *remoteTurn, b binding.Binding, reason string) (any, *rpcError) {
	r.meta.State, r.meta.Code, r.meta.Reason, r.meta.RequestRef = remoteNotSubmitted, "not_delivered", reason, ""
	text := notDeliveredText(b.Handle, reason)
	if dest := r.postChannel(); dest != "" {
		r.meta.Posted = s.publish(dest, text)
	}
	if r.settle("replied") == "client_disconnected" {
		return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: r.meta}}, nil
	}
	if err := emitText(r.emit, r.sessionID, "agent_message_chunk", text); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
	}
	return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: r.meta}}, nil
}

// drained reports whether the handle recorded a drained receipt for id. It
// stats the one receipt file instead of reading the receipts directory.
func drained(b binding.Binding, id string) bool {
	info, err := os.Lstat(filepath.Join(fsq.AgentReceipts(b.Root, b.Handle), id+"__"+b.Handle+"__"+receipt.StageDrained+".json"))
	return err == nil && info.Mode().IsRegular()
}

// replyHit is one inbox file whose header answers a prompt.
type replyHit struct {
	filename string
	header   format.Header
	created  time.Time
}

// inboxScan lists one inbox box and keeps each file's header by name. A
// delivered maildir file never changes, so each name's header is read once:
// a poll costs one directory listing plus the headers of names it has not
// seen, not a read of every file (review F2). A header that cannot be parsed
// is remembered as nil and the file is left for drain and the DLQ.
type inboxScan struct {
	dir     string
	headers map[string]*format.Header
}

func newInboxScan(dir string) *inboxScan {
	return &inboxScan{dir: dir, headers: map[string]*format.Header{}}
}

// replies refreshes the scan and returns its matches.
func (sc *inboxScan) replies(handle, threadID, promptID string, since time.Time) ([]replyHit, error) {
	if err := sc.refresh(); err != nil {
		return nil, err
	}
	return sc.match(handle, threadID, promptID, since), nil
}

// refresh lists the box once, reads the headers of names not seen yet, and
// forgets names that are gone. A missing dir is empty.
func (sc *inboxScan) refresh() error {
	entries, err := os.ReadDir(sc.dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		listed[name] = true
		if _, known := sc.headers[name]; known {
			continue
		}
		h, err := format.ReadHeaderFile(filepath.Join(sc.dir, name))
		var pathErr *fs.PathError
		switch {
		case err == nil:
			sc.headers[name] = &h
		case errors.As(err, &pathErr):
			// gone or unreadable now; try again on the next refresh
		default:
			sc.headers[name] = nil
		}
	}
	for name := range sc.headers {
		if !listed[name] {
			delete(sc.headers, name)
		}
	}
	return nil
}

// match returns, oldest first, the files from the last refresh that come
// from handle on threadID, ref promptID, and were created no earlier than
// since.
func (sc *inboxScan) match(handle, threadID, promptID string, since time.Time) []replyHit {
	var hits []replyHit
	for name, header := range sc.headers {
		if header == nil || !slices.Contains(header.Refs, promptID) {
			continue
		}
		if hit, ok := replyTo(name, header, handle, threadID, since); ok {
			hits = append(hits, hit)
		}
	}
	sortHits(hits)
	return hits
}

// replyTo reports whether header is a reply from handle on threadID created
// no earlier than since.
func replyTo(name string, header *format.Header, handle, threadID string, since time.Time) (replyHit, bool) {
	if header.From != handle || header.Thread != threadID {
		return replyHit{}, false
	}
	created, err := time.Parse(time.RFC3339Nano, header.Created)
	if err != nil || created.Before(since) {
		return replyHit{}, false
	}
	return replyHit{filename: name, header: *header, created: created}, true
}

// sortHits orders hits oldest first.
func sortHits(hits []replyHit) {
	slices.SortFunc(hits, func(a, b replyHit) int {
		if c := a.created.Compare(b.created); c != 0 {
			return c
		}
		return strings.Compare(a.filename, b.filename)
	})
}

// mailboxWatch reads one prompt's replies from the buzz inbox. Several
// bindings, processes and redeliveries share that inbox, so a reply is
// returned only by the watch that wins its forwarding claim.
type mailboxWatch struct {
	stateDir           string
	b                  binding.Binding
	threadID, promptID string
	created            time.Time
	inboxNew, inboxCur *inboxScan
	// adopt makes a reply already forwarded for this prompt count as this
	// watch's own: the late-reply sweep posts a reply whose turn forwarded
	// it but never posted it (a crash before the post). An adopting watch
	// skips status replies in cur, so no body is read on every sweep.
	adopt bool
}

func (s *Server) watchMailbox(b binding.Binding, threadID, promptID string, created time.Time) *mailboxWatch {
	return &mailboxWatch{
		stateDir: s.cfg.StateDir, b: b, threadID: threadID, promptID: promptID, created: created,
		inboxNew: newInboxScan(fsq.AgentInboxNew(b.Root, mailboxSender)),
		inboxCur: newInboxScan(fsq.AgentInboxCur(b.Root, mailboxSender)),
	}
}

// ownedReply is a reply this watch forwards, with its buzz inbox id.
type ownedReply struct {
	id  string
	msg format.Message
}

// poll claims the prompt's replies in buzz/inbox/new the way drain does (a
// move to cur and a drained receipt for consumer buzz) and never moves other
// files. withCur also recovers replies another consumer moved to cur before
// this watch saw them. A status reply is progress; the newest other reply
// is the final answer, returned with its id.
func (w *mailboxWatch) poll(withCur bool) (string, string, []string, error) {
	newHits, err := w.inboxNew.replies(w.b.Handle, w.threadID, w.promptID, w.created)
	if err != nil {
		return "", "", nil, err
	}
	var curHits []replyHit
	if withCur {
		if curHits, err = w.inboxCur.replies(w.b.Handle, w.threadID, w.promptID, w.created); err != nil {
			return "", "", nil, err
		}
	}
	return w.forward(newHits, curHits)
}

// forward claims the matched replies in new, recovers the matched replies
// in cur, and returns what this watch owns.
func (w *mailboxWatch) forward(newHits, curHits []replyHit) (string, string, []string, error) {
	var owned []ownedReply
	var root *fsq.DeliveryRoot
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()
	openRoot := func() (*fsq.DeliveryRoot, error) {
		if root != nil {
			return root, nil
		}
		identity, err := fsq.SnapshotDeliveryRoot(w.b.Root)
		if err != nil {
			return nil, err
		}
		root, err = fsq.OpenDeliveryRoot(w.b.Root, identity)
		return root, err
	}
	for _, hit := range newHits {
		dr, err := openRoot()
		if err != nil {
			return "", "", nil, err
		}
		msg, ok, err := w.claimNew(dr, hit)
		if err != nil {
			return "", "", nil, err
		}
		if ok {
			owned = append(owned, ownedReply{strings.TrimSuffix(hit.filename, ".md"), msg})
		}
	}
	for _, hit := range curHits {
		if w.adopt && hit.header.Kind == string(format.KindStatus) {
			continue
		}
		msg, ok, err := w.recover(hit.filename)
		if err != nil {
			return "", "", nil, err
		}
		if ok {
			dr, err := openRoot()
			if err != nil {
				return "", "", nil, err
			}
			drainedReceipt(dr, w.b.Handle, hit.header)
			owned = append(owned, ownedReply{strings.TrimSuffix(hit.filename, ".md"), msg})
		}
	}
	var progress []string
	finalID, final, newest := "", "", time.Time{}
	for _, o := range owned {
		msg := o.msg
		body := strings.TrimSpace(msg.Body)
		if body == "" {
			continue
		}
		if msg.Header.Kind == string(format.KindStatus) {
			progress = append(progress, w.b.Handle+": "+body)
			continue
		}
		if created, _ := time.Parse(time.RFC3339Nano, msg.Header.Created); final == "" || !created.Before(newest) {
			finalID, final, newest = o.id, body, created
		}
	}
	return finalID, final, progress, nil
}

// claimNew moves one reply from new to cur and forwards it. Only a
// successful move (or this move's own committed-durability error) owns the
// file: ENOENT means another consumer moved it first, and that reply is
// recovered from cur, by whichever watch wins the forwarding claim. The
// mover emits the drained receipt whoever wins forwarding, so a reply that
// a concurrent cur recovery forwards first still gets one.
func (w *mailboxWatch) claimNew(root *fsq.DeliveryRoot, hit replyHit) (format.Message, bool, error) {
	var committed *fsq.CommittedDurabilityError
	if err := fsq.MoveNewToCur(root, mailboxSender, hit.filename); err != nil && !errors.As(err, &committed) {
		if os.IsNotExist(err) {
			return format.Message{}, false, nil
		}
		return format.Message{}, false, err
	}
	drainedReceipt(root, w.b.Handle, hit.header)
	return w.recover(hit.filename)
}

// drainedReceipt records that consumer buzz drained a reply. It is
// idempotent (one file per message id) and best effort, as in drain: the
// claim already holds.
func drainedReceipt(root *fsq.DeliveryRoot, sender string, h format.Header) {
	_ = receipt.EmitDeliveryRoot(root, receipt.New(h.ID, h.Thread, sender, mailboxSender, receipt.StageDrained, ""))
}

// recover forwards one reply in buzz/inbox/cur if this watch wins its
// forwarding claim: an exclusive record keyed by queue root and file name,
// so each reply is returned at most once across turns and processes.
func (w *mailboxWatch) recover(filename string) (format.Message, bool, error) {
	key := sha256.Sum256([]byte(w.b.Root + "\x00" + filename))
	record := filepath.Join(w.stateDir, "forwarded", hex.EncodeToString(key[:]))
	// A reply forwarded earlier costs one small read on each heartbeat.
	owned := false
	if raw, err := readSmallRegular(record); err == nil {
		owned = w.adopt && strings.TrimSpace(string(raw)) == w.promptID
	} else if errors.Is(err, os.ErrNotExist) {
		if owned, err = createExclusive(record, []byte(w.promptID+"\n")); err != nil {
			return format.Message{}, false, err
		}
	} else {
		return format.Message{}, false, err
	}
	if !owned {
		return format.Message{}, false, nil
	}
	msg, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxCur(w.b.Root, mailboxSender), filename))
	if err != nil {
		if os.IsNotExist(err) {
			return format.Message{}, false, nil
		}
		return format.Message{}, false, err
	}
	return msg, true, nil
}
