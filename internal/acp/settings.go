package acp

import (
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

// loadTurnTiming reads the durations of a turn that is starting from the
// owner's settings file (remoteturn.Timings). The startup values are the
// configured durations at the first turn.
func (s *Server) loadTurnTiming() remoteturn.Timing {
	s.mu.Lock()
	if s.timings == nil {
		name := "amq-acp"
		if s.cfg.RemoteBinding {
			name = "machine agent"
		}
		s.timings = remoteturn.NewTimings(name, remoteturn.Timing{Timeout: s.cfg.TurnTimeout, Poll: s.cfg.PollInterval, Heartbeat: s.cfg.HeartbeatInterval})
	}
	timings := s.timings
	s.mu.Unlock()
	return timings.Load()
}

// timing is the turn's durations: those fixed at turn start, or the startup
// values for a turn built without a start (tests, internal callers).
func (s *Server) timing(turn *turnState) remoteturn.Timing {
	if turn != nil && turn.timing != nil {
		return *turn.timing
	}
	return remoteturn.Timing{Timeout: s.cfg.TurnTimeout, Poll: s.cfg.PollInterval, Heartbeat: s.cfg.HeartbeatInterval}
}
