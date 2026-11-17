package acp

import (
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

// loadTurnTiming reads the durations of a turn that is starting from the
// owner's settings file (remoteturn.Timings); the configured durations are
// its startup values.
func (s *Server) loadTurnTiming() remoteturn.Timing {
	return s.timings.Load(remoteturn.Timing{Timeout: s.cfg.TurnTimeout, Poll: s.cfg.PollInterval, Heartbeat: s.cfg.HeartbeatInterval})
}

// timing is the turn's durations: those fixed at turn start, or the startup
// values for a turn built without a start (tests, internal callers).
func (s *Server) timing(turn *turnState) remoteturn.Timing {
	if turn != nil && turn.timing != nil {
		return *turn.timing
	}
	return remoteturn.Timing{Timeout: s.cfg.TurnTimeout, Poll: s.cfg.PollInterval, Heartbeat: s.cfg.HeartbeatInterval}
}
