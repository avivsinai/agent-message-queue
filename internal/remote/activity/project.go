package activity

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

// observation is one native activity fact before encryption.
type observation struct {
	Seq       uint64
	Kind      string
	SessionID string
	TurnID    string
	Text      string
	At        time.Time
}

type observerJSON struct {
	Seq        uint64          `json:"seq"`
	Timestamp  string          `json:"timestamp"`
	Kind       string          `json:"kind"`
	AgentIndex int             `json:"agentIndex"`
	ChannelID  *string         `json:"channelId"`
	SessionID  string          `json:"sessionId"`
	TurnID     string          `json:"turnId,omitempty"`
	StartedAt  string          `json:"startedAt,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

func (o observation) marshal() (string, error) {
	payload, err := o.payload()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(observerJSON{
		Seq:        o.Seq,
		Timestamp:  o.At.UTC().Format(time.RFC3339Nano),
		Kind:       o.Kind,
		AgentIndex: 0,
		SessionID:  o.SessionID,
		TurnID:     o.TurnID,
		Payload:    payload,
	})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (o observation) payload() (json.RawMessage, error) {
	switch o.Kind {
	case "turn_started", "turn_completed":
		return json.RawMessage("{}"), nil
	case "session_resolved":
		return json.Marshal(map[string]any{
			"sessionId":    o.SessionID,
			"isNewSession": false,
		})
	case "acp_read":
		return acp.MarshalTextSessionUpdate(o.SessionID, "agent_message_chunk", o.Text, map[string]string{
			"provenance": "native_projection",
		})
	default:
		return nil, fmt.Errorf("activity kind %q", o.Kind)
	}
}

// projectCodex maps the Codex app-server notifications the attachment already
// understands. Other methods, including NIP-AO cancel_turn, are ignored.
func projectCodex(threadID string, n codex.Notification) (observation, bool) {
	switch n.Method {
	case "turn/started":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != threadID || p.Turn.ID == "" {
			return observation{}, false
		}
		return observation{Kind: "turn_started", SessionID: p.ThreadID, TurnID: p.Turn.ID}, true
	case "item/completed":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != threadID || p.Item.Type != "agentMessage" || p.Item.Text == "" {
			return observation{}, false
		}
		return observation{Kind: "acp_read", SessionID: p.ThreadID, TurnID: p.TurnID, Text: p.Item.Text}, true
	case "turn/completed":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != threadID || p.Turn.ID == "" {
			return observation{}, false
		}
		return observation{Kind: "turn_completed", SessionID: p.ThreadID, TurnID: p.Turn.ID}, true
	default:
		return observation{}, false
	}
}
