package activity

import (
	"encoding/json"
	"fmt"
	"strings"
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
	Update    string
	ToolID    string
	Title     string
	Status    string
	ItemID    string
	Plan      []planEntry
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
		meta := map[string]string{"provenance": "native_projection"}
		if o.ItemID != "" {
			meta["itemId"] = o.ItemID
		}
		switch o.Update {
		case "tool_call", "tool_call_update":
			return toolSessionUpdate(o, meta)
		case "plan":
			return planSessionUpdate(o, meta)
		default:
			update := o.Update
			if update == "" {
				update = "agent_message_chunk"
			}
			return acp.MarshalTextSessionUpdate(o.SessionID, update, o.Text, meta, o.ToolID)
		}
	default:
		return nil, fmt.Errorf("activity kind %q", o.Kind)
	}
}

// toolSessionUpdate is the ACP tool_call / tool_call_update notification.
// It is not the text-message codec: tool content is a ToolCallContent array,
// and status is the observed state (Desktop treats a missing status as completed).
func toolSessionUpdate(o observation, meta map[string]string) (json.RawMessage, error) {
	update := map[string]any{
		"sessionUpdate": o.Update,
		"toolCallId":    o.ToolID,
		"title":         o.Title,
		"status":        o.Status,
	}
	if o.Update == "tool_call_update" && o.Text != "" {
		update["content"] = []any{
			map[string]any{
				"type": "content",
				"content": map[string]any{
					"type": "text",
					"text": o.Text,
				},
			},
		}
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": o.SessionID,
			"update":    update,
			"_meta":     meta,
		},
	})
}

// planSessionUpdate is the ACP plan notification. The client replaces the
// whole plan on each update, so every frame carries every entry.
func planSessionUpdate(o observation, meta map[string]string) (json.RawMessage, error) {
	entries := o.Plan
	if entries == nil {
		entries = []planEntry{}
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": o.SessionID,
			"update": map[string]any{
				"sessionUpdate": "plan",
				"entries":       entries,
			},
			"_meta": meta,
		},
	})
}

// planEntry is one ACP PlanEntry. Codex steps carry no priority, and ACP
// requires one, so every entry is "medium".
type planEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// codexStep says what the sink does with one projected notification.
type codexStep int

const (
	// stepFrame queues the observation as it is.
	stepFrame codexStep = iota
	// stepTextDelta and stepOutputDelta carry one streamed piece in
	// obs.Text. The sink coalesces them before they become frames.
	stepTextDelta
	stepOutputDelta
	// stepMessageDone is a completed agentMessage with its whole text. The
	// sink queues it only when no delta streamed for the item.
	stepMessageDone
	// stepCommandStart is the tool_call for a command; obs.Title is the
	// command line.
	stepCommandStart
	// stepCommandDone is the final tool_call_update; obs.Text is the
	// aggregated output, empty when Codex sent none.
	stepCommandDone
	// stepTurnDone is turn_completed. It ends the turn's stream state.
	stepTurnDone
)

// codexFact is one Codex notification for the pinned thread, projected.
type codexFact struct {
	step codexStep
	obs  observation
}

// projectCodex maps the Codex app-server notifications that the activity
// view renders (app-server-protocol v2 at codex rust-v0.156.1). Other
// methods, and item types other than userMessage, agentMessage and
// commandExecution, are ignored.
func projectCodex(threadID string, n codex.Notification) (codexFact, bool) {
	switch n.Method {
	case "turn/started", "turn/completed":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != threadID || p.Turn.ID == "" {
			return codexFact{}, false
		}
		if n.Method == "turn/started" {
			return codexFact{step: stepFrame, obs: observation{Kind: "turn_started", SessionID: p.ThreadID, TurnID: p.Turn.ID}}, true
		}
		return codexFact{step: stepTurnDone, obs: observation{Kind: "turn_completed", SessionID: p.ThreadID, TurnID: p.Turn.ID}}, true
	case "item/started", "item/completed":
		var p struct {
			ThreadID string    `json:"threadId"`
			TurnID   string    `json:"turnId"`
			Item     codexItem `json:"item"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != threadID {
			return codexFact{}, false
		}
		return projectCodexItem(n.Method == "item/started", p.ThreadID, p.TurnID, p.Item)
	case "item/agentMessage/delta", "item/commandExecution/outputDelta":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != threadID || p.ItemID == "" || p.Delta == "" {
			return codexFact{}, false
		}
		obs := observation{Kind: "acp_read", SessionID: p.ThreadID, TurnID: p.TurnID, ItemID: p.ItemID, Text: p.Delta}
		if n.Method == "item/agentMessage/delta" {
			obs.Update = "agent_message_chunk"
			return codexFact{step: stepTextDelta, obs: obs}, true
		}
		obs.Update, obs.ToolID, obs.Status = "tool_call_update", p.ItemID, "in_progress"
		return codexFact{step: stepOutputDelta, obs: obs}, true
	case "turn/plan/updated":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Plan     []struct {
				Step   string `json:"step"`
				Status string `json:"status"`
			} `json:"plan"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != threadID {
			return codexFact{}, false
		}
		entries := make([]planEntry, 0, len(p.Plan))
		for _, step := range p.Plan {
			status := "pending"
			switch step.Status {
			case "inProgress":
				status = "in_progress"
			case "completed":
				status = "completed"
			}
			entries = append(entries, planEntry{Content: step.Step, Priority: "medium", Status: status})
		}
		return codexFact{step: stepFrame, obs: observation{Kind: "acp_read", Update: "plan", SessionID: p.ThreadID, TurnID: p.TurnID, Plan: entries}}, true
	default:
		return codexFact{}, false
	}
}

// codexItem is the subset of a v2 ThreadItem the projection reads.
type codexItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Command          string  `json:"command"`
	Status           string  `json:"status"`
	AggregatedOutput *string `json:"aggregatedOutput"`
}

func projectCodexItem(started bool, threadID, turnID string, item codexItem) (codexFact, bool) {
	obs := observation{Kind: "acp_read", SessionID: threadID, TurnID: turnID, ItemID: item.ID}
	switch {
	case item.Type == "userMessage" && !started:
		// Codex sends the same user item on start and on completion.
		// Only text inputs are shown; images and mentions are not.
		var parts []string
		for _, c := range item.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) == 0 {
			return codexFact{}, false
		}
		obs.Update, obs.Text = "user_message_chunk", strings.Join(parts, "\n")
		return codexFact{step: stepFrame, obs: obs}, true
	case item.Type == "agentMessage" && !started:
		obs.Update, obs.Text = "agent_message_chunk", item.Text
		return codexFact{step: stepMessageDone, obs: obs}, true
	case item.Type == "commandExecution" && item.ID != "":
		obs.ToolID, obs.Title = item.ID, item.Command
		if obs.Title == "" {
			obs.Title = item.ID
		}
		if started {
			obs.Update, obs.Status = "tool_call", "in_progress"
			return codexFact{step: stepCommandStart, obs: obs}, true
		}
		obs.Update = "tool_call_update"
		// ACP has no declined status; a declined command did not run, so it
		// shows as failed.
		switch item.Status {
		case "completed":
			obs.Status = "completed"
		case "inProgress":
			obs.Status = "in_progress"
		default:
			obs.Status = "failed"
		}
		if item.AggregatedOutput != nil {
			obs.Text = *item.AggregatedOutput
		}
		return codexFact{step: stepCommandDone, obs: obs}, true
	default:
		return codexFact{}, false
	}
}
