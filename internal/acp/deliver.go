package acp

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// CockpitPromptSubject labels prompts delivered on the durable cockpit thread.
const CockpitPromptSubject = "ACP cockpit prompt"

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
	return deliver(cfg, body, thread, CockpitPromptSubject, []string{"acp", "cockpit"}, eventID)
}

func deliver(cfg Config, body, thread, subject string, labels []string, eventID string) (Delivery, error) {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return Delivery{}, fmt.Errorf("prompt contains no text content")
	}
	if strings.TrimSpace(thread) == "" {
		return Delivery{}, fmt.Errorf("message thread is empty")
	}

	if eventID != "" {
		if existing, ok, err := loadEventRecord(cfg, eventID); err != nil {
			return Delivery{}, err
		} else if ok {
			out := existing.delivery()
			out.EventID = existing.EventID
			out.State = DeliveryDuplicate
			out.Committed = existing.Committed
			out.Drained = existing.Drained
			out.Started = existing.Started
			out.Completed = existing.Completed
			out.Egress = existing.Egress
			out.Duplicate = true
			// A replayed turn polls only for replies newer than this call.
			out.Created = time.Now()
			return out, nil
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
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    cfg.Me,
			To:      []string{cfg.To},
			Thread:  thread,
			Subject: subject,
			Created: now.UTC().Format(time.RFC3339Nano),
			Labels:  labels,
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

	_, err = fsq.DeliverToInboxes(root, []string{cfg.To}, id+".md", data)
	egress := EgressConfirmed
	if err != nil {
		var uncertain *fsq.CommittedDurabilityError
		if !errors.As(err, &uncertain) {
			return Delivery{}, err
		}
		egress = EgressUncertain
	}

	out := Delivery{
		MessageID: id,
		To:        cfg.To,
		Thread:    thread,
		Created:   now,
		EventID:   eventID,
		State:     DeliveryStateQueued,
		Committed: true,
		Drained:   false,
		Started:   false,
		Completed: false,
		Egress:    egress,
	}
	if eventID == "" {
		return out, nil
	}
	rec := eventRecord{
		Schema:    1,
		EventID:   eventID,
		MessageID: id,
		To:        cfg.To,
		Thread:    thread,
		Committed: true,
		Drained:   false,
		Started:   false,
		Completed: false,
		Egress:    egress,
	}
	if rememberErr := rememberEvent(cfg, rec); rememberErr != nil && !errors.Is(rememberErr, os.ErrExist) {
		return Delivery{}, rememberErr
	}
	return out, nil
}
