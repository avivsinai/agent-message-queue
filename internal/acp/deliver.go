package acp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// CockpitPromptSubject labels prompts delivered on the durable cockpit thread.
const CockpitPromptSubject = "ACP cockpit prompt"

// CockpitSteeringSubject labels steering delivered on the cockpit thread.
const CockpitSteeringSubject = "ACP steering"

// Delivery is the durable outcome of one prompt turn. The message is queued in
// the recipient's inbox; nothing here proves the recipient consumed it.
type Delivery struct {
	MessageID string
	To        string
	Thread    string
	Created   time.Time
	EventID   string
	State     string
	Committed bool
	Drained   bool
	Started   bool
	Completed bool
	Egress    string
	Duplicate bool
}

// DeliverCockpitPrompt sends a prompt on the stable cockpit thread used by the
// live ACP bridge. The returned creation time is the lower bound for reply
// polling, so an older message on the same thread cannot answer this turn.
func DeliverCockpitPrompt(cfg Config, body, thread, eventID string) (Delivery, error) {
	return deliver(cfg, body, thread, CockpitPromptSubject, format.PriorityNormal, []string{"acp", "cockpit"}, nil, eventID)
}

// DeliverSteering sends steering on the session's cockpit thread. During an
// in-flight turn (turnPrompt set) it is urgent with the buzz-steer label,
// AMQ's native interrupt signal, and refs the turn's prompt, so a peer's
// reply to the steer still answers that turn; while idle it is an ordinary
// normal-priority message.
func DeliverSteering(cfg Config, body, thread, turnPrompt, eventID string) (Delivery, error) {
	if turnPrompt == "" {
		return deliver(cfg, body, thread, CockpitSteeringSubject, format.PriorityNormal, []string{"acp"}, nil, eventID)
	}
	return deliver(cfg, body, thread, CockpitSteeringSubject, format.PriorityUrgent, []string{"acp", "buzz-steer"}, []string{turnPrompt}, eventID)
}

func deliver(cfg Config, body, thread, subject, priority string, labels, refs []string, eventID string) (Delivery, error) {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return Delivery{}, fmt.Errorf("prompt contains no text content")
	}
	if strings.TrimSpace(thread) == "" {
		return Delivery{}, fmt.Errorf("message thread is empty")
	}

	if eventID != "" {
		claim, err := loadEventRecord(cfg, eventID)
		if err != nil {
			return Delivery{}, err
		}
		if claim != nil {
			return deliverClaimed(cfg, *claim, body, subject, priority, refs)
		}
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return Delivery{}, err
	}
	if eventID != "" {
		labels = append(append([]string(nil), labels...), "nostr:"+eventID)
	}
	message := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     cfg.Me,
			To:       []string{cfg.To},
			Thread:   thread,
			Subject:  subject,
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: priority,
			Labels:   labels,
			Refs:     refs,
		},
		Body: body,
	}
	data, err := message.Marshal()
	if err != nil {
		return Delivery{}, err
	}
	if len(data) > format.MaxMessageSize {
		return Delivery{}, fmt.Errorf("prompt exceeds the maximum AMQ message size")
	}

	identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
	if err != nil {
		return Delivery{}, err
	}
	root, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
	if err != nil {
		return Delivery{}, err
	}
	defer func() { _ = root.Close() }()

	// The event journal is a claim taken before delivery, not a receipt
	// after it: like the mailbox steer claim it fixes the message id,
	// created time, and destination once, and records no delivery outcome.
	// With an event id it is created exclusively; a concurrent first
	// delivery that loses reads the winner's claim and follows the same
	// check-then-deliver path as a replay.
	if eventID != "" {
		rec := eventRecord{
			Schema:    1,
			EventID:   eventID,
			MessageID: id,
			To:        cfg.To,
			Thread:    thread,
			Created:   now.UTC().Format(time.RFC3339Nano),
		}
		if err := rememberEvent(cfg, rec); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return Delivery{}, err
			}
			// os.ErrExist: a concurrent winner claimed first; follow its claim
			claim, err := loadEventRecord(cfg, eventID)
			if err != nil {
				return Delivery{}, err
			}
			if claim == nil {
				return Delivery{}, fmt.Errorf("event %s lost its claim and the claim is unreadable", eventID)
			}
			return deliverClaimed(cfg, *claim, body, subject, priority, refs)
		}
	}

	return writeToInbox(cfg, root, claimIdentity(eventID, id, cfg.To, thread), now, data)
}

// claimIdentity builds the in-memory identity of a message being delivered
// first-hand, shaped like a claim so writeToInbox serves both paths.
func claimIdentity(eventID, messageID, to, thread string) *eventRecord {
	return &eventRecord{EventID: eventID, MessageID: messageID, To: to, Thread: thread}
}

// inboxHasMessage reports whether the recipient's inbox (new or cur) already
// holds the claimed message id — the only delivery proof a replay trusts.
func inboxHasMessage(root *fsq.DeliveryRoot, handle, messageID string) (bool, error) {
	name := messageID + ".md"
	for _, dir := range []string{fsq.AgentInboxNew(root.Base(), handle), fsq.AgentInboxCur(root.Base(), handle)} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// deliverClaimed serves a replay (or a concurrent loser) that follows an
// existing claim. The claim carries no outcome: delivery is what the
// recipient inbox proves. When the message is present the replay reports it
// (a duplicate on a replay); when it is absent the first attempt never
// reached the inbox, so the replay delivers with the claimed id and created
// time and reports what that write proves.
func deliverClaimed(cfg Config, claim eventRecord, body, subject, priority string, refs []string) (Delivery, error) {
	created, err := time.Parse(time.RFC3339Nano, claim.Created)
	if err != nil {
		return Delivery{}, fmt.Errorf("event %s has an unreadable claim time", claim.EventID)
	}
	identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
	if err != nil {
		return Delivery{}, err
	}
	root, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
	if err != nil {
		return Delivery{}, err
	}
	defer func() { _ = root.Close() }()

	delivered, err := inboxHasMessage(root, claim.To, claim.MessageID)
	if err != nil {
		return Delivery{}, err
	}
	if delivered {
		return Delivery{
			MessageID: claim.MessageID,
			To:        claim.To,
			Thread:    claim.Thread,
			Created:   created,
			EventID:   claim.EventID,
			State:     DeliveryDuplicate,
			Committed: true,
			Egress:    EgressConfirmed,
			Duplicate: true,
		}, nil
	}

	message := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       claim.MessageID,
			From:     cfg.Me,
			To:       []string{claim.To},
			Thread:   claim.Thread,
			Subject:  subject,
			Created:  claim.Created,
			Priority: priority,
			Labels:   []string{"nostr:" + claim.EventID},
			Refs:     refs,
		},
		Body: body,
	}
	data, err := message.Marshal()
	if err != nil {
		return Delivery{}, err
	}
	if len(data) > format.MaxMessageSize {
		return Delivery{}, fmt.Errorf("prompt exceeds the maximum AMQ message size")
	}
	return writeToInbox(cfg, root, &claim, created, data)
}

// writeToInbox delivers an already-serialized message into the recipient
// inbox and reports what the write proves; a CommittedDurabilityError stays
// uncertain egress, on the first attempt and on a replay alike.
func writeToInbox(cfg Config, root *fsq.DeliveryRoot, claim *eventRecord, created time.Time, data []byte) (Delivery, error) {
	if claim == nil {
		return Delivery{}, fmt.Errorf("writeToInbox requires a claim")
	}
	id, thread, eventID := claim.MessageID, claim.Thread, claim.EventID
	_, err := fsq.DeliverToInboxes(root, []string{cfg.To}, id+".md", data)
	egress := EgressConfirmed
	if err != nil {
		var uncertain *fsq.CommittedDurabilityError
		if !errors.As(err, &uncertain) {
			return Delivery{}, err
		}
		egress = EgressUncertain
	}
	return Delivery{
		MessageID: id,
		To:        cfg.To,
		Thread:    thread,
		Created:   created,
		EventID:   eventID,
		State:     DeliveryStateQueued,
		Committed: true,
		Egress:    egress,
	}, nil
}
