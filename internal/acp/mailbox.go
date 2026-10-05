package acp

import (
	"encoding/json"
	"errors"
	"fmt"
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

	// A turn cancelled before delivery delivers nothing (codex #895 P1 #3).
	if outcome := r.settle(""); outcome == "session_cancelled" || outcome == "client_disconnected" {
		return s.mailboxNotDelivered(r, outcome)
	}
	// The turn budget starts before the claim, so a publish-lock wait counts
	// against it (codex #895 r2 P1).
	budget := time.Now().Add(s.cfg.TurnTimeout)
	claim, err := s.mailboxClaim(eventID, threadID)
	if err != nil {
		return r.failed(remoteUncertain, err)
	}
	threadID = claim.Thread
	created, err := time.Parse(time.RFC3339Nano, claim.Created)
	if err != nil {
		return nil, newRPCError(codeInternalError, "mailbox claim time: %v", err)
	}
	r.meta.RequestRef = claim.MessageID
	if err := s.publishClaimed(r, budget, b, threadID, claim.MessageID, created, text); err != nil {
		if errors.Is(err, errStoppedBeforePublish) {
			return s.mailboxNotDelivered(r, r.settle(""))
		}
		if errors.Is(err, lock.ErrStopped) {
			if outcome := r.settle("reply_timeout"); outcome != "reply_timeout" {
				return s.mailboxNotDelivered(r, outcome)
			}
			r.meta.Reason = "reply_timeout"
			return r.say("reply_timeout", StopReasonRefusal, fmt.Sprintf("Not delivered to %s: the turn ran out of time before the message could be published.", b.Handle))
		}
		return r.failed(remoteUncertain, err)
	}
	r.meta.State = DeliveryStateQueued
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
	seen := map[string]bool{}
	for {
		if !read && drained(b, claim.MessageID) {
			read = true
			r.meta.State = "read"
			if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Read by %s.", b.Handle)); err != nil {
				return nil, newRPCError(codeInternalError, "emit ACP session update: %v", err)
			}
		}
		final, progress, err := mailboxReplies(b, threadID, claim.MessageID, created, seen)
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
			return r.say("replied", StopReasonEndTurn, final)
		}
		select {
		case <-turn.done:
			return s.mailboxStopped(r, r.settle(""), b)
		case <-deadline.C:
			if outcome := r.settle("reply_timeout"); outcome != "reply_timeout" {
				return s.mailboxStopped(r, outcome, b)
			}
			r.meta.Reason = "reply_timeout"
			return r.say("reply_timeout", StopReasonRefusal, fmt.Sprintf("No final reply from %s yet. The message stays in its AMQ inbox and may still be answered.", b.Handle))
		case <-poll.C:
		case <-heartbeat.C:
			if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Still waiting for a reply from %s.", b.Handle)); err != nil {
				return nil, newRPCError(codeInternalError, "emit ACP heartbeat: %v", err)
			}
		}
	}
}

// mailboxStopped ends a turn the client cancelled or left. A mailbox cannot
// recall a delivered message, so the reply says the work may still run,
// and the cancel is recorded so a redelivery does not publish again.
func (s *Server) mailboxStopped(r *remoteTurn, outcome string, b binding.Binding) (any, *rpcError) {
	r.meta.Reason = outcome
	if outcome == "client_disconnected" {
		return remotePromptResult{StopReason: StopReasonRefusal, Meta: remotePromptMeta{Remote: r.meta}}, nil
	}
	if r.eventID != "" {
		if err := s.recordEventCancel(r.eventID); err != nil {
			return nil, newRPCError(codeInternalError, "record cancel: %v", err)
		}
	}
	return r.say(outcome, StopReasonCancelled, fmt.Sprintf("Stopped waiting. The message stays in %s's AMQ inbox; %s may still act on it.", b.Handle, b.Handle))
}

// mailboxClaim returns the message id and time for this prompt. With an
// event id it is claimed exclusively before any publish, and a redelivery
// reuses it.
func (s *Server) mailboxClaim(eventID, threadID string) (mailboxClaim, error) {
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return mailboxClaim{}, err
	}
	fresh := mailboxClaim{MessageID: id, Created: now.UTC().Format(time.RFC3339Nano), Thread: threadID}
	if eventID == "" {
		return fresh, nil
	}
	path := filepath.Join(s.cfg.StateDir, "remote-events", eventID+".mailbox.json")
	raw, err := json.Marshal(fresh)
	if err != nil {
		return mailboxClaim{}, err
	}
	if _, err := createExclusive(path, raw); err != nil {
		return mailboxClaim{}, err
	}
	stored, err := readSmallRegular(path)
	if err != nil {
		return mailboxClaim{}, err
	}
	if err := fsq.SyncDir(filepath.Dir(path)); err != nil {
		return mailboxClaim{}, err
	}
	var claim mailboxClaim
	if err := json.Unmarshal(stored, &claim); err != nil || claim.MessageID == "" || claim.Thread == "" {
		return mailboxClaim{}, fmt.Errorf("event %s has an unreadable mailbox claim; refusing to publish", eventID)
	}
	return claim, nil
}

// errStoppedBeforePublish means the turn was cancelled or left before the
// message was published; nothing was delivered.
var errStoppedBeforePublish = errors.New("stopped before publish")

// publishClaimed publishes an event's claimed message under a per-event
// lock, so two retries cannot both pass the absence check and publish the
// same message twice (codex #895 P1 #2). The lock wait gives up on cancel,
// disconnect or the turn budget, and cancellation is checked again once the
// lock is held, so a cancelled prompt is never published (codex #895 r2 P1).
func (s *Server) publishClaimed(r *remoteTurn, budget time.Time, b binding.Binding, threadID, id string, created time.Time, text string) error {
	publish := func() error {
		if outcome := r.settle(""); outcome == "session_cancelled" || outcome == "client_disconnected" {
			return errStoppedBeforePublish
		}
		// An expired budget publishes nothing, also when the lock was free
		// or there is no event lock at all (codex #895 r3 P2).
		if !time.Now().Before(budget) {
			return lock.ErrStopped
		}
		return publishOnce(b, threadID, id, created, text)
	}
	if r.eventID == "" {
		return publish()
	}
	if !lock.AdvisoryLockAvailable() {
		return errors.New("refusing to publish a Buzz event without an advisory file lock")
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
	err := lock.WithExclusiveFileLockUntil(path, stopped, publish)
	if errors.Is(err, lock.ErrStopped) {
		if outcome := r.settle(""); outcome == "session_cancelled" || outcome == "client_disconnected" {
			return errStoppedBeforePublish
		}
	}
	return err
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
func publishOnce(b binding.Binding, threadID, id string, created time.Time, text string) error {
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
	identity, err := fsq.SnapshotDeliveryRoot(b.Root)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(b.Root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, err := fsq.DeliverToInboxes(root, []string{b.Handle}, name, data); err != nil {
		var uncertain *fsq.CommittedDurabilityError
		if !errors.As(err, &uncertain) {
			return err
		}
	}
	return nil
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

// scanReplies reads only dir (one inbox box) and returns, oldest first, the
// files from handle on threadID that ref promptID and were created no earlier
// than since. It reads headers only; the caller reads the body of a hit. A
// file whose header cannot be read is skipped and left in place for drain and
// the DLQ. A missing dir has no replies.
func scanReplies(dir, handle, threadID, promptID string, since time.Time) ([]replyHit, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var hits []replyHit
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		header, err := format.ReadHeaderFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if header.From != handle || header.Thread != threadID || !slices.Contains(header.Refs, promptID) {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, header.Created)
		if err != nil || created.Before(since) {
			continue
		}
		hits = append(hits, replyHit{filename: name, header: header, created: created})
	}
	slices.SortStableFunc(hits, func(a, b replyHit) int { return a.created.Compare(b.created) })
	return hits, nil
}

// mailboxReplies claims the replies from the handle in buzz/inbox/new that
// ref the prompt and were created no earlier than it, the way drain does: a
// move to cur and a drained receipt for consumer buzz. Unmatched files are
// never moved, because several bindings share the buzz mailbox. A status
// reply is progress, reported once; the newest other reply is the final
// answer. Mailbox mode needs no cur fallback: it claims its own matches.
func mailboxReplies(b binding.Binding, threadID, promptID string, created time.Time, seen map[string]bool) (string, []string, error) {
	hits, err := scanReplies(fsq.AgentInboxNew(b.Root, mailboxSender), b.Handle, threadID, promptID, created)
	if err != nil || len(hits) == 0 {
		return "", nil, err
	}
	identity, err := fsq.SnapshotDeliveryRoot(b.Root)
	if err != nil {
		return "", nil, err
	}
	root, err := fsq.OpenDeliveryRoot(b.Root, identity)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = root.Close() }()
	var progress []string
	final := ""
	for _, hit := range hits {
		// A failed rename with ENOENT means another process claimed the file
		// first; it is then read from cur like an own claim.
		var committed *fsq.CommittedDurabilityError
		if err := fsq.MoveNewToCur(root, mailboxSender, hit.filename); err != nil && !errors.As(err, &committed) && !os.IsNotExist(err) {
			return "", nil, err
		}
		msg, err := format.ReadMessageFileRoot(root, filepath.Join("agents", mailboxSender, "inbox", "cur", hit.filename))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", nil, err
		}
		// The receipt is best effort, as in drain: the claim already holds.
		_ = receipt.EmitDeliveryRoot(root, receipt.New(msg.Header.ID, msg.Header.Thread, b.Handle, mailboxSender, receipt.StageDrained, ""))
		body := strings.TrimSpace(msg.Body)
		if body == "" {
			continue
		}
		if msg.Header.Kind == string(format.KindStatus) {
			if !seen[msg.Header.ID] {
				seen[msg.Header.ID] = true
				progress = append(progress, b.Handle+": "+body)
			}
			continue
		}
		final = body
	}
	return final, progress, nil
}
