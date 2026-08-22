package acp

import (
	"fmt"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// PromptSubject labels prompts arriving over the ACP companion so an operator
// reading a mailbox can tell where the message came from.
const PromptSubject = "ACP prompt"

const (
	CockpitPromptSubject   = "ACP cockpit prompt"
	CockpitSteeringSubject = "ACP steering"
)

// Delivery is the durable outcome of one prompt turn. The message is queued in
// the recipient's inbox; nothing here proves the recipient consumed it.
type Delivery struct {
	MessageID string
	To        string
	Thread    string
	Created   time.Time
}

// DeliverPrompt writes the prompt text into the destination inbox using the
// ordinary Maildir tmp -> new delivery, so amq list and amq drain observe it
// exactly like any other message.
func DeliverPrompt(cfg Config, body string) (Delivery, error) {
	return deliver(cfg, body, p2pThread(cfg.Me, cfg.To), PromptSubject, format.PriorityNormal, []string{"acp"})
}

// DeliverCockpitPrompt sends a prompt on the stable channel thread used by the
// live ACP bridge. The returned creation time is the lower bound for reply
// polling, so an older message on the same channel cannot answer this turn.
func DeliverCockpitPrompt(cfg Config, body, thread string) (Delivery, error) {
	return deliver(cfg, body, thread, CockpitPromptSubject, format.PriorityNormal, []string{"acp", "cockpit"})
}

// DeliverSteering sends a steering message on the same channel thread. Urgent
// priority is the AMQ-native interrupt signal; the body remains explicit so a
// receiver that only reads message text still gets safe owner guidance.
func DeliverSteering(cfg Config, body, thread string, urgent bool) (Delivery, error) {
	priority := format.PriorityNormal
	labels := []string{"acp"}
	if urgent {
		priority = format.PriorityUrgent
		labels = append(labels, "buzz-steer")
	}
	return deliver(cfg, body, thread, CockpitSteeringSubject, priority, labels)
}

func deliver(cfg Config, body, thread, subject, priority string, labels []string) (Delivery, error) {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return Delivery{}, fmt.Errorf("prompt contains no text content")
	}
	if strings.TrimSpace(thread) == "" {
		return Delivery{}, fmt.Errorf("message thread is empty")
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return Delivery{}, err
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
			Labels:   append([]string(nil), labels...),
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

	if _, err := fsq.DeliverToInboxes(root, []string{cfg.To}, id+".md", data); err != nil {
		return Delivery{}, err
	}
	return Delivery{MessageID: id, To: cfg.To, Thread: thread, Created: now}, nil
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
