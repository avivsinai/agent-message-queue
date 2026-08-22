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

// PromptSubject labels prompts arriving over the ACP companion so an operator
// reading a mailbox can tell where the message came from.
const PromptSubject = "ACP prompt"

// Delivery is the durable outcome of one prompt turn. The message is queued in
// the recipient's inbox; nothing here proves the recipient consumed it.
type Delivery struct {
	MessageID string
	To        string
	Thread    string
	EventID   string
	State     string
	Committed bool
	Drained   bool
	Started   bool
	Completed bool
	Egress    string
	Duplicate bool
}

// DeliverPrompt writes the prompt text into the destination inbox using the
// ordinary Maildir tmp -> new delivery, so amq list and amq drain observe it
// exactly like any other message. Recipient and root always come from Config;
// prompt text, including a Buzz [Context] section, is never treated as
// authentication or a routing override.
func DeliverPrompt(cfg Config, body, eventID string) (Delivery, error) {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return Delivery{}, fmt.Errorf("prompt contains no text content")
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
			return out, nil
		}
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return Delivery{}, err
	}
	thread := p2pThread(cfg.Me, cfg.To)
	labels := []string{"acp"}
	if eventID != "" {
		labels = append(labels, "nostr:"+eventID)
	}
	message := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    cfg.Me,
			To:      []string{cfg.To},
			Thread:  thread,
			Subject: PromptSubject,
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

// p2pThread builds the documented canonical peer thread name so replies sent
// with amq reply and views from amq thread join the same conversation.
func p2pThread(a, b string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if b < a {
		a, b = b, a
	}
	return "p2p/" + a + "__" + b
}
