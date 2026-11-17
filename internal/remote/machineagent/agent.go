// Package machineagent is the machine agent engine that amq-remote serve
// runs: one Buzz identity on a relay transport that keeps a private channel
// per bound session, runs the owner's messages there as turns, posts
// dialogs, mirrors sessions, and posts late replies exactly once. New builds
// an agent, Start runs its workers, and Owner takes each owner message.
package machineagent

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

// A turn's durations when acp.settings sets none.
const (
	defaultTurnTimeout       = 10 * time.Minute
	defaultPollInterval      = 100 * time.Millisecond
	defaultHeartbeatInterval = 15 * time.Second
)

// Agent is one machine agent: its Buzz identity, its channels and the owner
// turns, dialogs and session mirrors it serves.
type Agent struct {
	// timings reads acp.settings for each turn that starts.
	timings *remoteturn.Timings
	// startup is a turn's durations when acp.settings sets none, and
	// postTimeout bounds one Buzz post.
	startup     remoteturn.Timing
	postTimeout time.Duration
	// stateDir is the agent's state directory.
	stateDir string
	mu       sync.Mutex
	sessions map[string]*sessionState
	// late is the late-reply sweep state.
	late lateReplySweep
	// id is the agent's Buzz identity (Config.Identity), fixed at
	// construction. Its owner is the only key whose reactions and messages
	// answer a dialog.
	id buzzop.Identity
	// leader releases the lifecycle lead of this worker's Buzz identity, or
	// is nil. Start and the machine worker set it; the workers' stop
	// releases it.
	leader func()
	// machine is the machine leader's pacing, used only by the machine
	// worker.
	machine machineState
	// buzz are the machine lifecycle's Buzz operations, posts the Buzz
	// writes and reads of dialogs, the session mirror and the hook warnings,
	// and direct the relay transport that serves both.
	buzz   lifecycle
	posts  posts
	direct Transport
	// working holds each route's working indication (*working);
	// workingFailed says the first failed pulse once.
	working       sync.Map
	workingFailed sync.Once
	// online reports whether the transport has a relay connection; nil
	// means it is not known, and the machine tick runs.
	online func() bool
	// owners are the agent's owner turns (Owner), run while its machine
	// workers run.
	owners ownerTurns
	// dialogs is the dialog poster's state, used only by the worker that
	// runs dialogTick.
	dialogs dialogState
	// mirrors is the session mirror's state, used only by the worker that
	// runs mirrorTick.
	mirrors mirrorState
	// view is the bindings and channel records the machine, dialog and
	// mirror workers share.
	view sessionView
	// targets caches the agent's dialog targets for the dialog and mirror
	// workers.
	targets targetCache
	// appServers judges Codex app-server threads for the current machine
	// tick, or is nil before the first one.
	appServers atomic.Pointer[appServerJudge]
}

// sessionState is the session of one route (routeSessionPrefix): its
// cockpit thread, its channel, and its in-flight turn.
type sessionState struct {
	thread string
	// channel is the route's Buzz channel: the owner's messages come from
	// it, and the owner-facing texts of its turns are posted there.
	channel string
	// turn is the in-flight owner turn, nil while idle.
	turn *turnState
}

// turnState is one owner turn. Its terminal outcome is decided exactly
// once, under Agent.mu, by whichever of reply, the workers' stop or timeout
// settles it first.
type turnState struct {
	// timing is set when the turn starts and fixed for the turn; nil means
	// the startup values.
	timing *remoteturn.Timing
	// done is closed when the turn is settled from outside the wait loop
	// (the workers' stop).
	done chan struct{}
	// outcome is turnOpen until settled.
	outcome turnOutcome
	// event is the owner's signed message the turn runs.
	event nostr.Event
}

// cockpitThread is the AMQ thread of the session sessionID.
func cockpitThread(sessionID string) string {
	return "cockpit/" + sessionID
}

// turnOutcome is how a turn settled (remoteturn.Outcome).
type turnOutcome = remoteturn.Outcome

const (
	turnOpen         = remoteturn.Open
	turnReplied      = remoteturn.Replied
	turnDisconnected = remoteturn.Disconnected
	turnTimedOut     = remoteturn.TimedOut
)

// settleLocked decides the turn's outcome if it is still open; it reports
// whether this call decided it. Caller holds s.mu.
func (t *turnState) settleLocked(outcome turnOutcome) bool {
	if t.outcome != turnOpen {
		return false
	}
	t.outcome = outcome
	return true
}

// settler is the Settle of a remote turn that runs as turn: it decides the
// turn's outcome under s.mu if it is still open and returns the outcome that
// stands. turnOpen only reads it.
func (s *Agent) settler(turn *turnState) func(turnOutcome) turnOutcome {
	return func(outcome turnOutcome) turnOutcome {
		s.mu.Lock()
		defer s.mu.Unlock()
		if outcome != turnOpen {
			turn.settleLocked(outcome)
		}
		return turn.outcome
	}
}

// beginTurnLocked starts a turn in the session sessionID. Caller holds s.mu.
func (s *Agent) beginTurnLocked(sessionID string) (*sessionState, *turnState, error) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, nil, fmt.Errorf("no session %q", sessionID)
	}
	if session.turn != nil {
		return nil, nil, fmt.Errorf("session %q has a turn in flight", sessionID)
	}
	session.turn = &turnState{done: make(chan struct{})}
	return session, session.turn, nil
}

// finishTurn clears the session's turn only if it is still this turn, so an
// old turn's cleanup can never clear a newer one.
func (s *Agent) finishTurn(session *sessionState, turn *turnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.turn == turn {
		session.turn = nil
	}
}

// loadTurnTiming reads the durations of a turn that is starting from the
// owner's settings file (remoteturn.Timings); the configured durations are
// its startup values.
func (s *Agent) loadTurnTiming() remoteturn.Timing {
	return s.timings.Load(s.startup)
}

// timing is the turn's durations: those fixed at turn start, or the startup
// values for a turn built without a start (tests, internal callers).
func (s *Agent) timing(turn *turnState) remoteturn.Timing {
	if turn != nil && turn.timing != nil {
		return *turn.timing
	}
	return s.startup
}
