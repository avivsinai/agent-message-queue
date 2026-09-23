package activity

import (
	"context"
	"fmt"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// AcceptParsed maps one transcript line the Claude adapter already parsed.
// SessionID is the cursor's session. TurnID is the adapter's user or
// absorbed boundary, never the line UUID. A meta line, a line for another
// native session, or a line with no user, assistant, tool_use, or
// tool_result block is ignored. Claude has no cancel here.
func (s *Sink) AcceptParsed(ctx context.Context, note claude.ActivityNote) error {
	if s.Publish == nil {
		return fmt.Errorf("activity publish is not set")
	}
	line := note.Line
	if line.Meta || (line.Type != "user" && line.Type != "assistant") {
		return nil
	}
	session := note.SessionID
	if session == "" {
		session = line.SessionID
	}
	if session == "" || session != s.ThreadID {
		return nil
	}
	var queued bool
	for _, block := range line.Blocks {
		obs, ok := projectClaudeBlock(session, note.TurnID, line, block)
		if !ok {
			continue
		}
		if obs.At.IsZero() {
			obs.At = s.now()
		}
		s.mu.Lock()
		emitReady := !s.sawSession && s.ThreadID != ""
		if emitReady {
			s.sawSession = true
		}
		s.mu.Unlock()
		if emitReady {
			ready := observation{Kind: "session_resolved", SessionID: s.ThreadID, At: obs.At}
			if err := s.queue(ready); err != nil {
				s.mu.Lock()
				s.sawSession = false
				s.mu.Unlock()
				return err
			}
		}
		if err := s.queue(obs); err != nil {
			return err
		}
		queued = true
	}
	if !queued {
		return nil
	}
	return s.Drain(ctx)
}

// AcceptClaude parses one transcript line with the adapter's parser and
// projects it. Production delivery uses AcceptParsed on the note the
// poller already decoded, so the JSONL is not read a second time. A raw
// line carries no turn; the line UUID stays the message identity.
func (s *Sink) AcceptClaude(ctx context.Context, line string) error {
	parsed, ok := claude.ParseTranscriptLine(line)
	if !ok {
		return nil
	}
	return s.AcceptParsed(ctx, claude.ActivityNote{Line: parsed, SessionID: parsed.SessionID})
}

func projectClaudeBlock(session, turnID string, line claude.TranscriptLine, block claude.TranscriptBlock) (observation, bool) {
	obs := observation{
		Kind:      "acp_read",
		SessionID: session,
		TurnID:    turnID,
		ItemID:    line.UUID,
	}
	if line.TS != 0 {
		obs.At = time.UnixMilli(line.TS).UTC()
	}
	switch block.Type {
	case "text":
		if block.Text == "" {
			return observation{}, false
		}
		obs.Text = block.Text
		if line.Type == "assistant" {
			obs.Update = "agent_message_chunk"
		} else {
			obs.Update = "user_message_chunk"
		}
	case "tool_use":
		if line.Type != "assistant" || (block.Name == "" && block.ID == "") {
			return observation{}, false
		}
		obs.Update = "tool_call"
		obs.Title = block.Name
		if obs.Title == "" {
			obs.Title = block.ID
		}
		obs.ToolID = block.ID
		obs.Status = "pending"
	case "tool_result":
		if line.Type != "user" || (block.ID == "" && block.Text == "" && !block.Failed) {
			return observation{}, false
		}
		obs.Update = "tool_call_update"
		obs.Text = block.Text
		obs.ToolID = block.ID
		obs.Title = block.ID
		obs.Status = "completed"
		if block.Failed {
			obs.Status = "failed"
		}
	default:
		return observation{}, false
	}
	return obs, true
}
