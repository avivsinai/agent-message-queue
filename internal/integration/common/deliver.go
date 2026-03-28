package common

import (
	"fmt"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// DeliverIntegrationMessage builds and delivers a standard integration message
// to the specified recipient's inbox. It uses the same Maildir atomic delivery
// as the rest of AMQ.
//
// Parameters:
//   - root: AMQ root directory
//   - from: sender handle
//   - to: recipient handle (single; self-delivery for hooks)
//   - subject: message subject line
//   - body: markdown body
//   - ctx: message context (should contain "orchestrator" key)
//   - labels: message labels
//   - thread: thread ID (e.g. "task/<workspace_key>")
//   - kind: message kind (e.g. "status", "todo")
//   - priority: message priority (e.g. "normal", "low")
func DeliverIntegrationMessage(root, from, to, subject, body string, ctx map[string]interface{}, labels []string, thread, kind, priority string) (string, error) {
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return "", fmt.Errorf("generate message id: %w", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     from,
			To:       []string{to},
			Thread:   thread,
			Subject:  subject,
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: priority,
			Kind:     kind,
			Labels:   labels,
			Context:  ctx,
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return "", fmt.Errorf("marshal message: %w", err)
	}

	filename := id + ".md"
	paths, err := fsq.DeliverToInboxes(root, msg.Header.To, filename, data)
	if err != nil {
		return "", fmt.Errorf("deliver message: %w", err)
	}
	return paths[to], nil
}
