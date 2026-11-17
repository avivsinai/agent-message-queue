package machineagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/mailprompt"
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

// mailboxSender is the AMQ handle Buzz prompts come from in a mailbox
// binding. The bound agent answers it with `amq reply`.
const mailboxSender = "buzz"

// mailboxSubject labels Buzz prompts in the bound agent's inbox. Agents and
// the amq-remote skill match on it, so it is a fixed routing key.
const mailboxSubject = "Buzz DM"

// mailboxClaim is the durable first-delivery record of one event in a
// mailbox binding: the message id and creation time chosen once, so a
// redelivery publishes the same message at most once and waits for the
// same reply.
type mailboxClaim struct {
	MessageID string `json:"message_id"`
	Created   string `json:"created"`
	// Thread is the cockpit thread of the first delivery. A redelivery
	// publishes and waits there.
	Thread string `json:"thread"`
	// Channel is the Buzz channel of the first delivery. The turn and the
	// late-reply sweep post the event's texts there.
	Channel string `json:"channel,omitempty"`
	// Deadline is when the first turn's budget ends, so the sweep knows when
	// that turn is over even if the turn timeout was changed since. An older
	// claim has none and the sweep uses the startup turn timeout.
	Deadline string `json:"deadline,omitempty"`
}

// mailboxOutcome is the content of an event's final-answer marker
// <event>.posted.final. The turn or the late-reply sweep creates it before
// it posts the final reply (ReplyID names that buzz inbox message). The
// exclusive create decides which outcome stands, once, across processes.
type mailboxOutcome struct {
	ReplyID string `json:"reply_id,omitempty"`
	// Cancelled is read, never written: a stored record that carries it
	// still decodes as a decided event.
	Cancelled bool `json:"cancelled,omitempty"`
	// NotSubmitted records that the handle's inbox refused the message after
	// the publish lock proved it absent: nothing was delivered, and a
	// redelivery of the event ends here instead of publishing it later.
	NotSubmitted bool   `json:"not_submitted,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// damaged is set when the marker exists but holds no readable outcome.
	// The event stays decided, so nothing is delivered or posted again, but
	// the turn reports an uncertain outcome, never a reply.
	damaged error
}

// runMailbox delivers the prompt as an AMQ message to the bound handle and
// holds the turn until that handle answers. A reply of kind status is
// progress; any other reply that refs the prompt is the final answer. How
// the handle notices the message (a wake, a built-in consumer, monitor, or
// its next drain) is the handle owner's business, not this bridge's. The
// turn is an owner turn of the machine agent: it holds the owner's signed
// event, eventID is that event's id, and the owner reads every text as a
// post in the event's channel.
func (s *Agent) runMailbox(sessionID, channel, text, eventID string, b binding.Binding, turn *turnState) (remoteturn.Result, error) {
	r := s.newBoundTurn(eventID, channel, turn)
	r.signed, r.Meta = eventContext(turn.event), remoteturn.Meta{Target: b.Handle}
	s.mu.Lock()
	threadID := ""
	if session, ok := s.sessions[sessionID]; ok {
		threadID = session.thread
	}
	s.mu.Unlock()
	if threadID == "" {
		threadID = cockpitThread("session/" + sessionID)
	}
	r.MailboxThread = threadID

	// A turn stopped before delivery delivers nothing.
	if outcome := r.Settle(turnOpen); outcome.Stopped() {
		return s.mailboxStopped(r, outcome)
	}
	// The turn budget starts before the claim, so a publish-lock wait counts
	// against it.
	budget := time.Now().Add(s.timing(turn).Timeout)
	claim, err := s.publishClaimed(r, budget, b, threadID, text)
	if claim.MessageID != "" {
		threadID = claim.Thread
		r.MailboxThread = threadID
		r.Meta.RequestRef = claim.MessageID
		// The event's texts go to the channel of its first delivery.
		r.mailbox, r.claimChannel = true, claim.Channel
		s.late.add(eventID)
	}
	if err != nil {
		if errors.Is(err, errStoppedBeforePublish) {
			return s.mailboxStopped(r, r.Settle(turnOpen))
		}
		if errors.Is(err, lock.ErrStopped) {
			if outcome := r.Settle(turnTimedOut); outcome != turnTimedOut {
				return s.mailboxStopped(r, outcome)
			}
			// This attempt published nothing, but a replay may follow an
			// earlier delivery. A timeout before acquiring the lock has not
			// even read the claim, so absence is never proven for an event.
			return r.Failed(remoteUncertain, errors.New("the turn ran out of time before delivery could be confirmed"))
		}
		if reason, refused := mailprompt.NotDeliveredReason(err); refused {
			return s.mailboxRefused(r, b, reason)
		}
		// A redelivered event that is already answered returns that answer
		// and posts nothing.
		if errors.Is(err, errEventDecided) {
			if out, ok := s.mailboxAnswered(eventID); ok {
				return s.mailboxAnsweredTurn(r, out)
			}
		}
		return r.Failed(remoteUncertain, err)
	}
	created, err := time.Parse(time.RFC3339Nano, claim.Created)
	if err != nil {
		return remoteturn.Result{}, fmt.Errorf("mailbox claim time: %v", err)
	}
	r.Meta.State = remoteturn.StateQueued

	deadline := time.NewTimer(time.Until(budget))
	defer deadline.Stop()
	poll := time.NewTicker(s.timing(turn).Poll)
	defer poll.Stop()
	heartbeat := time.NewTicker(s.timing(turn).Heartbeat)
	defer heartbeat.Stop()
	read := false
	// The turn start and each heartbeat also recover replies another
	// consumer moved to cur; other polls read only new.
	watch, withCur := s.watchMailbox(b, threadID, claim.MessageID, created), true
	for {
		if !read && drained(b, claim.MessageID) {
			read = true
			r.Meta.State = "read"
		}
		// The sweep or another process may have claimed and posted the reply.
		if out, ok := s.mailboxAnswered(eventID); ok {
			return s.mailboxAnsweredTurn(r, out)
		}
		finalID, final, progress, err := watch.poll(withCur)
		withCur = false
		if err != nil {
			return remoteturn.Result{}, fmt.Errorf("poll AMQ thread %s: %v", threadID, err)
		}
		for _, note := range progress {
			// The owner reads a status reply only as a post.
			s.postProgress(eventID, r.postChannel(), note)
		}
		if final != "" {
			if outcome := r.Settle(turnReplied); outcome != turnReplied {
				return s.mailboxStopped(r, outcome)
			}
			r.Meta.State = remoteturn.StateReplied
			return s.sayReply(r, finalID, final)
		}
		select {
		case <-turn.done:
			return s.mailboxStopped(r, r.Settle(turnOpen))
		case <-deadline.C:
			// An outcome decided since the loop's last check stands: the
			// status post is refused once the event has its final marker.
			if outcome := r.Settle(turnTimedOut); outcome != turnTimedOut {
				return s.mailboxStopped(r, outcome)
			}
			r.Meta.Reason = string(turnTimedOut)
			return r.Say(postStatus, turnTimedOut, remoteturn.StopReasonRefusal, fmt.Sprintf("No final reply from %s yet. The message stays in its AMQ inbox and may still be answered.", b.Handle))
		case <-poll.C:
		case <-heartbeat.C:
			withCur = true
		}
	}
}

// mailboxStopped ends a turn that stopped before it was answered: the
// machine workers stopped, and the turn was cut as a client that left. It
// posts nothing and records nothing, so a later delivery of the event runs
// the turn again.
func (s *Agent) mailboxStopped(r *boundTurn, outcome turnOutcome) (remoteturn.Result, error) {
	r.Meta.Reason = string(outcome)
	return remoteturn.Result{StopReason: remoteturn.StopReasonRefusal, Meta: remoteturn.ResultMeta{Remote: r.Meta}}, nil
}

// mailboxAnsweredTurn ends a turn whose event already has an outcome and
// posts nothing.
func (s *Agent) mailboxAnsweredTurn(r *boundTurn, out mailboxOutcome) (remoteturn.Result, error) {
	if out.damaged != nil {
		return r.Failed(remoteUncertain, out.damaged)
	}
	if outcome := r.Settle(turnReplied); outcome != turnReplied {
		return s.mailboxStopped(r, outcome)
	}
	return s.answeredResult(r, out)
}

// answeredResult is the result of a turn whose event already has its
// outcome: a reply, or a message that was not delivered. Whoever decided
// that outcome posted it; this turn posts nothing.
func (s *Agent) answeredResult(r *boundTurn, out mailboxOutcome) (remoteturn.Result, error) {
	if out.damaged != nil {
		return r.Failed(remoteUncertain, out.damaged)
	}
	r.Meta.Posted = "duplicate: this event already has its final outcome"
	if out.NotSubmitted {
		r.Meta.State, r.Meta.Code, r.Meta.Reason, r.Meta.RequestRef = remoteNotSubmitted, "not_delivered", out.Reason, ""
		return remoteturn.Result{StopReason: remoteturn.StopReasonRefusal, Meta: remoteturn.ResultMeta{Remote: r.Meta}}, nil
	}
	r.Meta.State, r.Meta.Reason = remoteturn.StateReplied, ""
	return remoteturn.Result{StopReason: remoteturn.StopReasonEndTurn, Meta: remoteturn.ResultMeta{Remote: r.Meta}}, nil
}

// decideOutcome makes proposed the event's one durable outcome, unless
// another is already recorded, and returns the outcome that stands and
// whether proposed won. The exclusive create of <event>.posted.final is the
// single decision point for a final post (a turn's or the sweep's) and a
// refused delivery, across processes.
func (s *Agent) decideOutcome(eventID string, proposed mailboxOutcome) (mailboxOutcome, bool, error) {
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
	return final, won, nil
}

// sayReply posts the final reply once per event, in the channel of the
// event's first delivery. If the event already has an outcome, or another
// reply wins it now, the turn returns that outcome and posts nothing.
func (s *Agent) sayReply(r *boundTurn, replyID, text string) (remoteturn.Result, error) {
	final, won, err := s.decideOutcome(r.EventID, mailboxOutcome{ReplyID: replyID})
	switch {
	case err != nil:
		r.Meta.Posted = "error: record post marker: " + err.Error()
	case !won:
		return s.answeredResult(r, final)
	default:
		r.Meta.Posted = s.publishFinal(r.EventID, r.postChannel(), text)
	}
	return remoteturn.Result{StopReason: remoteturn.StopReasonEndTurn, Meta: remoteturn.ResultMeta{Remote: r.Meta}}, nil
}

// mailboxAnswered returns the event's outcome once its final-answer marker
// exists. The marker holds an outcome record, or one timestamp line (a final
// text posted with no reply id: the remote path, and mailbox builds before
// outcome records). Anything else is a damaged marker: the event counts as
// decided, with damaged set.
func (s *Agent) mailboxAnswered(eventID string) (mailboxOutcome, bool) {
	raw, err := readSmallRegular(filepath.Join(s.eventDir(), eventID+".posted."+postFinal))
	if errors.Is(err, os.ErrNotExist) {
		return mailboxOutcome{}, false
	}
	if err != nil {
		return mailboxOutcome{damaged: fmt.Errorf("the event's final marker is unreadable: %w", err)}, true
	}
	if _, err := time.Parse(time.RFC3339Nano, strings.TrimSuffix(string(raw), "\n")); err == nil {
		return mailboxOutcome{}, true
	}
	var out mailboxOutcome
	if err := json.Unmarshal(raw, &out); err != nil || (out.ReplyID == "" && !out.Cancelled && !out.NotSubmitted) {
		return mailboxOutcome{damaged: errors.New("the event's final marker records no outcome")}, true
	}
	return out, true
}

// mailboxClaim returns the message id and time for the event's prompt, and
// whether this call created the claim. The claim is created exclusively
// under the publish lock, and a redelivery reuses it.
func (s *Agent) mailboxClaim(eventID, threadID, channel string, deadline time.Time) (mailboxClaim, bool, error) {
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return mailboxClaim{}, false, err
	}
	fresh := mailboxClaim{MessageID: id, Created: now.UTC().Format(time.RFC3339Nano), Thread: threadID, Channel: channel, Deadline: deadline.UTC().Format(time.RFC3339Nano)}
	path := filepath.Join(s.eventDir(), eventID+".mailbox.json")
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

// postProgress posts one status reply into channel, never after the event's
// final text. The forwarding claim (mailboxWatch.recover) hands each reply
// over once.
func (s *Agent) postProgress(eventID, channel, text string) {
	_ = s.withPostLock(eventID, func() error {
		if _, final := s.mailboxAnswered(eventID); !final {
			s.publish(channel, text)
		}
		return nil
	})
}

// errStoppedBeforePublish means the turn stopped before the message was
// published; nothing was delivered.
var errStoppedBeforePublish = errors.New("stopped before publish")

// errEventDecided means the event already had its outcome when the publish
// lock was held, so nothing was published; the turn adopts that outcome.
var errEventDecided = errors.New("event already has its outcome")

// publishClaimed claims the event's message and publishes it under a
// per-event lock, so two retries cannot both pass the absence check and
// publish the same message twice. It returns the claim once it was read. The
// lock wait gives up when the turn stops or its budget ends, and the stop is
// checked again once the lock is held, so a stopped turn publishes nothing.
func (s *Agent) publishClaimed(r *boundTurn, budget time.Time, b binding.Binding, threadID, text string) (mailboxClaim, error) {
	var claim mailboxClaim
	publish := func() error {
		// The claim is created under the lock, so only the attempt that
		// created it can know no earlier attempt published it.
		c, fresh, err := s.mailboxClaim(r.EventID, threadID, r.channel, budget)
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
		if _, ok := s.mailboxAnswered(r.EventID); ok {
			return errEventDecided
		}
		if outcome := r.Settle(turnOpen); outcome.Stopped() {
			return errStoppedBeforePublish
		}
		// An expired budget publishes nothing, also when the lock was free.
		if !time.Now().Before(budget) {
			return lock.ErrStopped
		}
		err = publishOnce(b, c.Thread, c.MessageID, r.EventID, r.signed, created, text)
		reason, refused := mailprompt.NotDeliveredReason(err)
		if !refused {
			return err
		}
		// An earlier attempt may have delivered the message before the
		// mailbox went away, so a redelivery's refusal stays unknown.
		if !fresh {
			return errors.New(reason)
		}
		// Record the refusal as the event's outcome under the publish lock,
		// so no redelivery can publish between the check and the record.
		_, won, decideErr := s.decideOutcome(r.EventID, mailboxOutcome{NotSubmitted: true, Reason: reason})
		switch {
		case decideErr != nil:
			return decideErr
		case !won:
			return errEventDecided
		}
		return err
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
	path := filepath.Join(s.eventDir(), r.EventID+".mailbox.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return claim, err
	}
	err := lock.WithExclusiveFileLockUntil(path, stopped, publish)
	if errors.Is(err, lock.ErrStopped) {
		if outcome := r.Settle(turnOpen); outcome.Stopped() {
			return claim, errStoppedBeforePublish
		}
	}
	return claim, err
}

// publishOnce delivers a binding's Buzz prompt to the handle through
// mailprompt.Publish. The message is labelled nostr:<eventID>, the id of
// the owner's event, and signed, that event's header context, goes into the
// header unchanged.
func publishOnce(b binding.Binding, threadID, id, eventID string, signed map[string]any, created time.Time, text string) error {
	identity, err := fsq.SnapshotDeliveryRoot(b.Root)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(b.Root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return mailprompt.Publish(root, id, created, mailprompt.Prompt{
		To:      b.Handle,
		From:    mailboxSender,
		Thread:  threadID,
		Subject: mailboxSubject,
		Labels:  []string{"acp", "buzz", "nostr:" + eventID},
		Context: signed,
		Body: strings.TrimRight(text, "\n") + "\n\n" +
			"This came from the owner in this session's Buzz channel. Answer with `amq reply --id " + id + "`; your reply is shown there. " +
			"A reply with `--kind status` shows as progress; any other reply is the final answer.",
	})
}

// eventContext is the header context {"buzz": {"event": <evt>}} of a
// verified event, or nil, logged, when it does not encode.
func eventContext(evt nostr.Event) map[string]any {
	raw, err := evt.MarshalJSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "machine agent: event %s is delivered without its signed event: %v\n", evt.ID.Hex(), err)
		return nil
	}
	return map[string]any{"buzz": map[string]any{"event": json.RawMessage(raw)}}
}

// notDeliveredText is the reply for an event whose message never reached
// the handle's inbox.
func notDeliveredText(handle, reason string) string {
	return fmt.Sprintf("Not delivered to %s: %s. Nothing reached its inbox; send it again.", handle, reason)
}

// mailboxRefused ends the turn that recorded the event's not-submitted
// outcome. That record is the at-most-once guard for the post, as for a
// final reply.
func (s *Agent) mailboxRefused(r *boundTurn, b binding.Binding, reason string) (remoteturn.Result, error) {
	r.Meta.State, r.Meta.Code, r.Meta.Reason, r.Meta.RequestRef = remoteNotSubmitted, "not_delivered", reason, ""
	text := notDeliveredText(b.Handle, reason)
	if dest := r.postChannel(); dest != "" {
		r.Meta.Posted = s.publish(dest, text)
	}
	r.Settle(turnReplied)
	return remoteturn.Result{StopReason: remoteturn.StopReasonRefusal, Meta: remoteturn.ResultMeta{Remote: r.Meta}}, nil
}

// drained reports whether the handle recorded a drained receipt for id. It
// stats the one receipt file instead of reading the receipts directory.
func drained(b binding.Binding, id string) bool {
	info, err := os.Lstat(filepath.Join(fsq.AgentReceipts(b.Root, b.Handle), id+"__"+b.Handle+"__"+receipt.StageDrained+".json"))
	return err == nil && info.Mode().IsRegular()
}

// mailboxWatch reads one prompt's replies from the buzz inbox. Several
// bindings, processes and redeliveries share that inbox, so a reply is
// returned only by the watch that wins its forwarding claim.
type mailboxWatch struct {
	stateDir           string
	b                  binding.Binding
	threadID, promptID string
	created            time.Time
	inboxNew, inboxCur *mailprompt.Scan
	// adopt makes a reply already forwarded for this prompt count as this
	// watch's own: the late-reply sweep posts a reply whose turn forwarded
	// it but never posted it (a crash before the post). An adopting watch
	// skips status replies in cur, so no body is read on every sweep.
	adopt bool
}

func (s *Agent) watchMailbox(b binding.Binding, threadID, promptID string, created time.Time) *mailboxWatch {
	return &mailboxWatch{
		stateDir: s.cfg.StateDir, b: b, threadID: threadID, promptID: promptID, created: created,
		inboxNew: mailprompt.NewScan(fsq.AgentInboxNew(b.Root, mailboxSender)),
		inboxCur: mailprompt.NewScan(fsq.AgentInboxCur(b.Root, mailboxSender)),
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
	newHits, err := w.inboxNew.Replies(w.b.Handle, w.threadID, w.promptID, w.created)
	if err != nil {
		return "", "", nil, err
	}
	var curHits []mailprompt.Hit
	if withCur {
		if curHits, err = w.inboxCur.Replies(w.b.Handle, w.threadID, w.promptID, w.created); err != nil {
			return "", "", nil, err
		}
	}
	return w.forward(newHits, curHits)
}

// forward claims the matched replies in new, recovers the matched replies
// in cur, and returns what this watch owns.
func (w *mailboxWatch) forward(newHits, curHits []mailprompt.Hit) (string, string, []string, error) {
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
			owned = append(owned, ownedReply{strings.TrimSuffix(hit.Filename, ".md"), msg})
		}
	}
	for _, hit := range curHits {
		if w.adopt && hit.Header.Kind == string(format.KindStatus) {
			continue
		}
		msg, ok, err := w.recover(hit.Filename)
		if err != nil {
			return "", "", nil, err
		}
		if ok {
			dr, err := openRoot()
			if err != nil {
				return "", "", nil, err
			}
			_ = drainedReceipt(dr, mailboxSender, hit.Header)
			owned = append(owned, ownedReply{strings.TrimSuffix(hit.Filename, ".md"), msg})
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
func (w *mailboxWatch) claimNew(root *fsq.DeliveryRoot, hit mailprompt.Hit) (format.Message, bool, error) {
	if moved, err := moveReply(root, mailboxSender, hit.Filename); err != nil || !moved {
		return format.Message{}, false, err
	}
	_ = drainedReceipt(root, mailboxSender, hit.Header)
	return w.recover(hit.Filename)
}

// moveReply moves one reply from consumer's inbox/new to cur the way drain
// does. moved is false when another consumer moved it first (ENOENT); a
// committed move whose sync failed is this caller's move.
func moveReply(root *fsq.DeliveryRoot, consumer, filename string) (bool, error) {
	var committed *fsq.CommittedDurabilityError
	if err := fsq.MoveNewToCur(root, consumer, filename); err != nil && !errors.As(err, &committed) {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// drainedReceipt records that consumer drained the reply h. It is
// idempotent: one file per message id.
func drainedReceipt(root *fsq.DeliveryRoot, consumer string, h format.Header) error {
	return receipt.EmitDeliveryRoot(root, receipt.New(h.ID, h.Thread, h.From, consumer, receipt.StageDrained, ""))
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
