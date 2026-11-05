package codex

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RolloutLine is the part of one rollout JSONL line that the session mirror
// reads. Codex writes a rollout file for every non-ephemeral thread:
//
//	{"timestamp":"...","type":"event_msg","payload":{"type":"task_started",...}}
//
// The parse keeps only names, ids, the user's and the assistant's text and
// the end of a turn. It has no field for a command, its arguments or
// output, a file change or a tool input, so those are never decoded.
type RolloutLine struct {
	// Type is the line type (session_meta, event_msg, response_item, ...).
	// Event is the payload type of an event_msg line.
	Type  string
	Event string
	// TS is the line's time in unix milliseconds, 0 when it has none.
	TS int64
	// TurnID is the turn an event belongs to; "" when it names none.
	TurnID string
	// Trigger is what started a task_started turn (turn_attribution).
	Trigger string
	// Error is set on a task_complete that carries an error.
	Error bool
	// LastAgentMessage is a task_complete's final assistant text.
	LastAgentMessage string
	// HistoryMode is a session_meta line's history mode.
	HistoryMode string
	// Item is the item of an item_completed event.
	Item RolloutItem
}

// RolloutItem is the part of a completed turn item the mirror reads.
type RolloutItem struct {
	// Type is the item variant: UserMessage, AgentMessage, CommandExecution,
	// FileChange, McpToolCall, ...
	Type string
	ID   string
	// Text is a UserMessage's first text input, or an AgentMessage's text.
	Text string
	// Phase is an AgentMessage's phase (commentary, final_answer).
	Phase string
	// ClientID is a UserMessage's client_id: the clientUserMessageId of the
	// request that sent it.
	ClientID string
	// Source is a CommandExecution's source (agent, user_shell, ...).
	Source string
	// Server and Tool name an McpToolCall; Tool alone a DynamicToolCall or
	// CollabAgentToolCall. Kind is an Extension item's kind.
	Server string
	Tool   string
	Kind   string
}

// ParseRolloutLine parses one rollout line; false when it is not JSON.
func ParseRolloutLine(line string) (RolloutLine, bool) {
	var raw struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type            string `json:"type"`
			TurnID          string `json:"turn_id"`
			TurnAttribution *struct {
				TurnTrigger string `json:"turn_trigger"`
			} `json:"turn_attribution"`
			Error            json.RawMessage `json:"error"`
			LastAgentMessage *string         `json:"last_agent_message"`
			HistoryMode      string          `json:"history_mode"`
			Item             *struct {
				Type     string          `json:"type"`
				ID       string          `json:"id"`
				ClientID string          `json:"client_id"`
				Content  json.RawMessage `json:"content"`
				Phase    json.RawMessage `json:"phase"`
				Source   json.RawMessage `json:"source"`
				Server   json.RawMessage `json:"server"`
				Tool     json.RawMessage `json:"tool"`
				Kind     json.RawMessage `json:"kind"`
			} `json:"item"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return RolloutLine{}, false
	}
	p := raw.Payload
	l := RolloutLine{Type: raw.Type, TurnID: p.TurnID}
	if ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp); err == nil {
		l.TS = ts.UnixMilli()
	}
	switch raw.Type {
	case "session_meta":
		l.HistoryMode = p.HistoryMode
		return l, true
	case "event_msg":
	default:
		return l, true
	}
	l.Event = p.Type
	if p.TurnAttribution != nil {
		l.Trigger = p.TurnAttribution.TurnTrigger
	}
	l.Error = len(p.Error) > 0 && string(p.Error) != "null"
	if p.LastAgentMessage != nil {
		l.LastAgentMessage = *p.LastAgentMessage
	}
	if it := p.Item; it != nil {
		l.Item = RolloutItem{Type: it.Type, ID: it.ID}
		switch it.Type {
		case "UserMessage":
			l.Item.ClientID = it.ClientID
			l.Item.Text = firstText(it.Content)
		case "AgentMessage":
			l.Item.Text = allText(it.Content)
			l.Item.Phase = rawString(it.Phase)
		case "CommandExecution":
			l.Item.Source = rawString(it.Source)
		case "McpToolCall":
			l.Item.Server, l.Item.Tool = rawString(it.Server), rawString(it.Tool)
		case "DynamicToolCall", "CollabAgentToolCall":
			l.Item.Tool = rawString(it.Tool)
		case "Extension":
			l.Item.Kind = rawString(it.Kind)
		}
	}
	return l, true
}

// textInput is one text entry of a message's content: a user input
// ("text") or an agent message part ("Text").
type textInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textInputs(raw json.RawMessage) []string {
	var parts []textInput
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		if strings.EqualFold(p.Type, "text") {
			out = append(out, p.Text)
		}
	}
	return out
}

func firstText(raw json.RawMessage) string {
	if texts := textInputs(raw); len(texts) > 0 {
		return texts[0]
	}
	return ""
}

func allText(raw json.RawMessage) string {
	return strings.TrimSpace(strings.Join(textInputs(raw), "\n"))
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// rolloutHeadMax bounds the first line read to find a rollout's history
// mode; the session_meta line carries the session's base instructions.
const rolloutHeadMax = 4 << 20

// Rollout is the rollout file of thread under codexHome:
// sessions/YYYY/MM/DD/rollout-<time>-<thread>.jsonl, when exactly one
// matches and its session_meta line names the paginated history mode, the
// one that keeps completed items. "" otherwise.
func Rollout(codexHome, thread string) string {
	if codexHome == "" || thread == "" || strings.ContainsAny(thread, `/\*?[`) {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if err != nil || len(matches) != 1 {
		return ""
	}
	path := matches[0]
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(io.LimitReader(f, rolloutHeadMax))
	head, err := r.ReadString('\n')
	if err != nil {
		return ""
	}
	l, ok := ParseRolloutLine(strings.TrimSpace(head))
	if !ok || l.Type != "session_meta" || l.HistoryMode != "paginated" {
		return ""
	}
	return path
}
