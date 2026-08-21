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

// Delivery is the durable outcome of one prompt turn. The message is queued in
// the recipient's inbox; nothing here proves the recipient consumed it.
type Delivery struct {
	MessageID string
	To        string
	Thread    string
}

// DeliverPrompt writes the prompt text into the destination inbox using the
// ordinary Maildir tmp -> new delivery, so amq list and amq drain observe it
// exactly like any other message.
func DeliverPrompt(cfg Config, body string) (Delivery, error) {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return Delivery{}, fmt.Errorf("prompt contains no text content")
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return Delivery{}, err
	}
	thread := p2pThread(cfg.Me, cfg.To)
	message := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    cfg.Me,
			To:      []string{cfg.To},
			Thread:  thread,
			Subject: PromptSubject,
			Created: now.UTC().Format(time.RFC3339Nano),
			Labels:  []string{"acp"},
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
	return Delivery{MessageID: id, To: cfg.To, Thread: thread}, nil
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
