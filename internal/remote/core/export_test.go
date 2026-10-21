package core

import "github.com/avivsinai/agent-message-queue/internal/remote/requests"

// Observe registers a callback for every record write and every storage
// failure projection, so a test can collect state history.
func (e *Endpoint) Observe(fn func(*requests.Record)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observers = append(e.observers, fn)
}

// InFlight returns the current number of in-flight handlers, so a test can
// observe the drain state without a sleep.
func (e *Endpoint) InFlight() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inFlight
}

// IsDraining reports whether the endpoint has begun shutdown (state is
// draining or closed).
func (e *Endpoint) IsDraining() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state != stateAccepting
}

// IsDrainingState reports whether the endpoint is in the draining state
// specifically, not closed: Close is still waiting on in-flight handlers.
func (e *Endpoint) IsDrainingState() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == stateDraining
}
