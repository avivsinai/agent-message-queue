package activity

import (
	"context"
	"fmt"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
)

// AcceptPi maps one activity record the pi adapter tailed from the bridge.
// A record for another native session, with an unknown kind, or with no
// text where it needs one is ignored.
func (s *Sink) AcceptPi(ctx context.Context, note pi.ActivityNote) error {
	if s.Publish == nil {
		return fmt.Errorf("activity publish is not set")
	}
	if note.SessionID == "" || note.SessionID != s.ThreadID {
		return nil
	}
	obs, ok := projectPi(note)
	if !ok {
		return nil
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
	return s.Drain(ctx)
}

func projectPi(note pi.ActivityNote) (observation, bool) {
	obs := observation{Kind: "acp_read", SessionID: note.SessionID, TurnID: note.TurnID, ItemID: note.ID}
	if note.At != 0 {
		obs.At = time.UnixMilli(note.At).UTC()
	}
	switch note.Kind {
	case "turn_start":
		obs.Kind = "turn_started"
	case "turn_end":
		obs.Kind = "turn_completed"
	case "user", "assistant":
		if note.Text == "" {
			return observation{}, false
		}
		obs.Text = note.Text
		obs.Update = "agent_message_chunk"
		if note.Kind == "user" {
			obs.Update = "user_message_chunk"
		}
	case "tool_start":
		if note.ID == "" && note.Tool == "" {
			return observation{}, false
		}
		obs.Update, obs.ToolID, obs.Title, obs.Status = "tool_call", note.ID, note.Tool, "pending"
		if obs.Title == "" {
			obs.Title = note.ID
		}
	case "tool_end":
		if note.ID == "" && note.Text == "" && note.Status != "failed" {
			return observation{}, false
		}
		obs.Update, obs.ToolID, obs.Title, obs.Text, obs.Status = "tool_call_update", note.ID, note.Tool, note.Text, "completed"
		if obs.Title == "" {
			obs.Title = note.ID
		}
		if note.Status == "failed" {
			obs.Status = "failed"
		}
	default:
		return observation{}, false
	}
	return obs, true
}
