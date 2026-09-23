package activity

import (
	"context"
	"fmt"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// AcceptClaude maps one line the Claude adapter already parsed. A meta line,
// a line for another native session, or a line with no user, assistant,
// tool_use, or tool_result block is ignored. Claude has no cancel here.
func (s *Sink) AcceptClaude(ctx context.Context, line string) error {
	if s.Publish == nil {
		return fmt.Errorf("activity publish is not set")
	}
	parsed, ok := claude.ParseTranscriptLine(line)
	if !ok || parsed.Meta {
		return nil
	}
	if parsed.SessionID != "" && parsed.SessionID != s.ThreadID {
		return nil
	}
	if parsed.Type != "user" && parsed.Type != "assistant" {
		return nil
	}
	var queued bool
	for _, block := range parsed.Blocks {
		obs, ok := projectClaudeBlock(s.ThreadID, parsed, block)
		if !ok {
			continue
		}
		if obs.At.IsZero() {
			obs.At = s.now()
		}
		if !s.sawSession && s.ThreadID != "" {
			ready := observation{Kind: "session_resolved", SessionID: s.ThreadID, At: obs.At}
			if err := s.queue(ready); err != nil {
				return err
			}
			s.sawSession = true
		}
		if err := s.queue(obs); err != nil {
			return err
		}
		queued = true
	}
	if !queued {
		return nil
	}
	return s.flush(ctx)
}

func projectClaudeBlock(session string, line claude.TranscriptLine, block claude.TranscriptBlock) (observation, bool) {
	obs := observation{Kind: "acp_read", SessionID: session, TurnID: line.UUID}
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
		obs.Text = block.Name
		obs.ToolID = block.ID
	case "tool_result":
		if line.Type != "user" || (block.ID == "" && block.Text == "") {
			return observation{}, false
		}
		obs.Update = "tool_call_update"
		obs.Text = block.Text
		obs.ToolID = block.ID
	default:
		return observation{}, false
	}
	return obs, true
}
