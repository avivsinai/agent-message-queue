package ack

import (
	"encoding/json"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// Ack records that a message was received and acknowledged.
type Ack struct {
	Schema   int    `json:"schema"`
	MsgID    string `json:"msg_id"`
	Thread   string `json:"thread"`
	From     string `json:"from"`
	To       string `json:"to"`
	Received string `json:"received"`
}

func New(msgID, thread, from, to string, received time.Time) Ack {
	return Ack{
		Schema:   format.CurrentSchema,
		MsgID:    msgID,
		Thread:   thread,
		From:     from,
		To:       to,
		Received: received.UTC().Format(time.RFC3339Nano),
	}
}

func (a Ack) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
