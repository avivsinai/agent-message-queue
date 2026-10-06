package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// mailboxSteerTarget is where a follow-up DM goes while a mailbox turn is
// open: the bound handle in the binding's root, on the thread and with refs
// to the prompt message that turn published. event is the turn's Buzz event.
type mailboxSteerTarget struct {
	binding binding.Binding
	thread  string
	prompt  string
	event   string
}

// steerClaim is the durable first-delivery record of one steer event,
// <steer-event>.steer.json in the shared state dir. Like the prompt claim it
// fixes the message id, time and destination once, so a replayed steer goes
// where the first one went, never into the session's current turn
// (review of #965, finding 1).
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

// openMailboxSteering makes a published mailbox turn steerable. A turn is
// steerable only after its prompt is in the handle's inbox, so a follow-up
// never reaches the handle before the prompt it refs.
func (s *Server) openMailboxSteering(turn *turnState, b binding.Binding, thread, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turn.mailboxSteer = &mailboxSteerTarget{binding: b, thread: thread, prompt: prompt, event: turn.mailboxEvent}
}

// errMailboxSteering is the refusal for a steer in binding mode that has no
// open, published mailbox turn to join: an idle session, a native binding,
// a prompt not yet delivered, or an event that already has its final
// answer. It keeps the amq-remote refusal code, on which buzz-acp falls back
// to its own cancel and merge.
func errMailboxSteering() *rpcError {
	return newRPCError(codeMethodNotFound, "steering needs an open mailbox turn in amq-remote mode")
}

// errSteerTurnOver means the parent event already has its final answer.
var errSteerTurnOver = errors.New("the turn already has its final answer")

// mailboxSteering delivers a follow-up DM into an open mailbox turn
// (bead agent-message-queue-cfy). It is an urgent buzz-steer message from
// buzz to the bound handle on the turn's thread that refs the turn's prompt,
// so `amq reply` to it also answers the turn. The turn keeps waiting for its
// final reply.
//
// The parent event's post lock serializes the steer with that event's final
// answer across processes: a steer is written only while the event has no
// final marker (review of #965, finding 2). A steer event's claim is created
// under that lock before the write; a replay follows the claim and writes
// only if the message is not already in the handle's inbox.
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
	var target *mailboxSteerTarget
	if turn := session.turn; turn != nil && turn.outcome == "" && turn.mailboxSteer != nil {
		copied := *turn.mailboxSteer
		target = &copied
	}
	s.mu.Unlock()

	// A replayed steer event follows its claim, whatever turn is open now.
	claim, replay, err := s.loadSteerClaim(eventID)
	if err != nil {
		return nil, newRPCError(codeInternalError, "steer claim: %v", err)
	}
	if !replay && target == nil {
		return nil, errMailboxSteering()
	}
	parent := claim.ParentEvent
	if !replay {
		parent = target.event
	}
	var delivery Delivery
	err = s.withEventPostLock(parent, func() error {
		if !replay {
			if s.eventFinal(parent) {
				return errSteerTurnOver
			}
			if claim, replay, err = s.claimSteer(eventID, *target); err != nil {
				return err
			}
		}
		// A replay whose message is missing writes it only while its
		// event is still open.
		stillOpen := func() bool { return !replay || !s.eventFinal(claim.ParentEvent) }
		delivery, err = writeSteerOnce(claim, eventID, text, stillOpen)
		return err
	})
	switch {
	case errors.Is(err, errSteerTurnOver):
		return nil, errMailboxSteering()
	case err != nil:
		return nil, newRPCError(codeInternalError, "deliver steering to %s: %v", claim.Handle, err)
	}
	outcome := SteeringInjected
	if replay {
		delivery.State, delivery.Duplicate = DeliveryDuplicate, true
		outcome = SteeringDuplicate
	}
	return newSteeringResult(outcome, delivery), nil
}

// withEventPostLock runs fn under the event's post lock (remote-events/
// <event>.post.lock), the lock the final answer is reserved under. A turn
// with no event has no final marker and no lock.
func (s *Server) withEventPostLock(eventID string, fn func() error) error {
	if eventID == "" {
		return fn()
	}
	if !lock.AdvisoryLockAvailable() {
		return errors.New("refusing to steer a Buzz event without an advisory file lock")
	}
	return s.withPostLock(eventID, fn)
}

// eventFinal reports whether the event has its final marker. An unreadable
// marker counts as final.
func (s *Server) eventFinal(eventID string) bool {
	if eventID == "" {
		return false
	}
	_, final := s.mailboxAnswered(eventID)
	return final
}

func (s *Server) steerClaimPath(eventID string) string {
	return filepath.Join(s.cfg.StateDir, "remote-events", eventID+".steer.json")
}

// loadSteerClaim reads the claim of a steer event, if one exists.
func (s *Server) loadSteerClaim(eventID string) (steerClaim, bool, error) {
	if eventID == "" {
		return steerClaim{}, false, nil
	}
	raw, err := readSmallRegular(s.steerClaimPath(eventID))
	if errors.Is(err, os.ErrNotExist) {
		return steerClaim{}, false, nil
	} else if err != nil {
		return steerClaim{}, false, err
	}
	var claim steerClaim
	if err := json.Unmarshal(raw, &claim); err != nil || claim.MessageID == "" || claim.Root == "" || claim.Thread == "" || claim.Prompt == "" || fsq.ValidateHandle(claim.Handle) != nil {
		return steerClaim{}, false, fmt.Errorf("steer event %s has an unreadable claim; refusing to deliver", eventID)
	}
	if _, err := time.Parse(time.RFC3339Nano, claim.Created); err != nil {
		return steerClaim{}, false, fmt.Errorf("steer event %s has an unreadable claim time", eventID)
	}
	// A claim can be visible before its directory entry is durable; make
	// it durable before anything is written on its behalf.
	if err := fsq.SyncDir(filepath.Dir(s.steerClaimPath(eventID))); err != nil {
		return steerClaim{}, false, err
	}
	return claim, true, nil
}

// claimSteer fixes a steer's message id and destination. With an event id
// it is created exclusively; a concurrent first delivery that loses reads
// the winner's claim and reports a replay.
func (s *Server) claimSteer(eventID string, target mailboxSteerTarget) (steerClaim, bool, error) {
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return steerClaim{}, false, err
	}
	claim := steerClaim{
		MessageID:   id,
		Created:     now.UTC().Format(time.RFC3339Nano),
		Binding:     target.binding.Name,
		Root:        target.binding.Root,
		Handle:      target.binding.Handle,
		Thread:      target.thread,
		Prompt:      target.prompt,
		ParentEvent: target.event,
	}
	if eventID == "" {
		return claim, false, nil
	}
	raw, err := json.Marshal(claim)
	if err != nil {
		return steerClaim{}, false, err
	}
	won, err := createExclusive(s.steerClaimPath(eventID), raw)
	if err != nil {
		return steerClaim{}, false, err
	}
	if won {
		return claim, false, nil
	}
	stored, ok, err := s.loadSteerClaim(eventID)
	if err == nil && !ok {
		err = fmt.Errorf("steer event %s lost its claim", eventID)
	}
	return stored, true, err
}

// writeSteerOnce delivers the claimed steer unless its message is already
// in the handle's inbox (new or cur), so a replay never duplicates it, and
// only if mayWrite still allows it. It writes no event journal: the claim
// is the steer's idempotency record, and a committed write is a delivery
// whatever happens after it (review of #965, finding 3).
func writeSteerOnce(claim steerClaim, eventID, text string, mayWrite func() bool) (Delivery, error) {
	created, _ := time.Parse(time.RFC3339Nano, claim.Created)
	out := Delivery{
		MessageID: claim.MessageID,
		To:        claim.Handle,
		Thread:    claim.Thread,
		Created:   created,
		EventID:   eventID,
		State:     DeliveryStateQueued,
		Committed: true,
		Egress:    EgressConfirmed,
	}
	identity, err := fsq.SnapshotDeliveryRoot(claim.Root)
	if err != nil {
		return Delivery{}, err
	}
	root, err := fsq.OpenDeliveryRoot(claim.Root, identity)
	if err != nil {
		return Delivery{}, err
	}
	defer func() { _ = root.Close() }()
	name := claim.MessageID + ".md"
	for _, dir := range []string{fsq.AgentInboxNew(claim.Root, claim.Handle), fsq.AgentInboxCur(claim.Root, claim.Handle)} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return out, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return Delivery{}, err
		}
	}
	if !mayWrite() {
		return Delivery{}, errSteerTurnOver
	}
	labels := []string{"acp", "buzz-steer"}
	if eventID != "" {
		labels = append(labels, "nostr:"+eventID)
	}
	body := formatSteeringBody(text) + "\n\nThis follow-up came from the owner's Buzz DM. Keep answering that DM with `amq reply --id " + claim.Prompt + "`."
	message := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       claim.MessageID,
			From:     mailboxSender,
			To:       []string{claim.Handle},
			Thread:   claim.Thread,
			Subject:  CockpitSteeringSubject,
			Created:  claim.Created,
			Priority: format.PriorityUrgent,
			Labels:   labels,
			Refs:     []string{claim.Prompt},
		},
		Body: strings.TrimRight(body, "\n"),
	}
	data, err := message.Marshal()
	if err != nil {
		return Delivery{}, err
	}
	if len(data) > format.MaxMessageSize {
		return Delivery{}, fmt.Errorf("steer exceeds the maximum AMQ message size")
	}
	if _, err := fsq.DeliverToInboxes(root, []string{claim.Handle}, name, data); err != nil {
		var uncertain *fsq.CommittedDurabilityError
		if !errors.As(err, &uncertain) {
			return Delivery{}, err
		}
		out.Egress = EgressUncertain
	}
	return out, nil
}
