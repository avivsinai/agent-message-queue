package supervisor

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

const (
	ActionBackoff     = "backoff"
	ActionDetached    = "detached"
	ActionEnsured     = "ensured"
	ActionStartFailed = "start_failed"
)

type Adapter interface {
	Probe(ctx context.Context, target string) error
}

type WakeRunner interface {
	StartWake(ctx context.Context, req amq.StartWakeRequest) error
}

type Reconciler struct {
	Wake        WakeRunner
	Adapter     Adapter
	Now         func() time.Time
	BackoffBase time.Duration
	BackoffMax  time.Duration
	Jitter      func(time.Duration) time.Duration
	InjectVia   string
	WakeTimeout time.Duration
}

type Result struct {
	Action     string
	AMQTouched bool
	Error      error
}

func (r Reconciler) Reconcile(ctx context.Context, entry registry.Entry) (registry.Entry, Result) {
	now := r.now()
	entry.LastSeenBySupervisor = now

	if blocked, result, ok := r.checkLocalReadiness(ctx, entry, now); ok {
		return blocked, result
	}

	// The registry target is authoritative. StartWake uses AMQ's target-aware
	// --accept-existing-wake path, which removes stale locks, starts missing
	// wakes, accepts an exact live target, and rejects a different live target.
	// Calling wake repair first could resurrect an obsolete persisted adapter.
	return r.ensureWake(ctx, entry, now)
}

func (r Reconciler) StartFresh(ctx context.Context, entry registry.Entry) (registry.Entry, Result) {
	now := r.now()
	entry.LastSeenBySupervisor = now

	if blocked, result, ok := r.checkLocalReadiness(ctx, entry, now); ok {
		return blocked, result
	}

	return r.ensureWake(ctx, entry, now)
}

func (r Reconciler) checkLocalReadiness(ctx context.Context, entry registry.Entry, now time.Time) (registry.Entry, Result, bool) {
	if !entry.BackoffUntil.IsZero() && now.Before(entry.BackoffUntil) {
		entry.LastSupervisorDecision = ActionBackoff
		return entry, Result{Action: ActionBackoff}, true
	}

	if r.Adapter == nil {
		updated, result := r.markBackoff(entry, now, errors.New("adapter is not configured"), ActionBackoff, false)
		return updated, result, true
	}
	if err := r.Adapter.Probe(ctx, entry.Target); err != nil {
		entry.State = registry.StateDetached
		entry.LastError = err.Error()
		entry.LastSupervisorDecision = ActionDetached
		return entry, Result{Action: ActionDetached, Error: err}, true
	}

	if r.Wake == nil {
		updated, result := r.markBackoff(entry, now, errors.New("amq runner is not configured"), ActionBackoff, false)
		return updated, result, true
	}
	return entry, Result{}, false
}

func (r Reconciler) ensureWake(ctx context.Context, entry registry.Entry, now time.Time) (registry.Entry, Result) {
	err := r.Wake.StartWake(ctx, amq.StartWakeRequest{
		Root:      entry.Root,
		Me:        entry.Agent,
		InjectVia: r.InjectVia,
		Adapter:   entry.Adapter,
		Target:    entry.Target,
		Timeout:   r.WakeTimeout,
	})
	if err == nil {
		return markActive(entry, now, ActionEnsured), Result{Action: ActionEnsured, AMQTouched: true}
	}
	return r.markBackoff(entry, now, err, ActionStartFailed, true)
}

func (r Reconciler) markBackoff(entry registry.Entry, now time.Time, err error, action string, amqTouched bool) (registry.Entry, Result) {
	entry.State = registry.StateAttached
	entry.FailureCount++
	entry.BackoffUntil = now.Add(r.backoff(entry.FailureCount))
	entry.LastSupervisorDecision = action
	if err != nil {
		entry.LastError = err.Error()
	}
	return entry, Result{Action: action, AMQTouched: amqTouched, Error: err}
}

func markActive(entry registry.Entry, now time.Time, action string) registry.Entry {
	entry.State = registry.StateActive
	entry.LastSeenBySupervisor = now
	entry.FailureCount = 0
	entry.BackoffUntil = time.Time{}
	entry.LastError = ""
	entry.LastSupervisorDecision = action
	return entry
}

func (r Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r Reconciler) backoff(failureCount int) time.Duration {
	base := r.BackoffBase
	if base <= 0 {
		base = time.Second
	}
	maxDelay := r.BackoffMax
	if maxDelay <= 0 {
		maxDelay = time.Minute
	}
	if failureCount < 1 {
		failureCount = 1
	}
	delay := base
	for i := 1; i < failureCount; i++ {
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}
	if delay > maxDelay {
		return maxDelay
	}
	return r.jitter(delay, maxDelay)
}

func (r Reconciler) jitter(delay time.Duration, maxDelay time.Duration) time.Duration {
	if r.Jitter != nil {
		jittered := r.Jitter(delay)
		if jittered < 0 {
			return 0
		}
		if jittered > maxDelay {
			return maxDelay
		}
		return jittered
	}
	if delay <= 0 {
		return delay
	}
	window := delay / 5
	if window <= 0 {
		return delay
	}
	span := int64(window*2) + 1
	offset, err := rand.Int(rand.Reader, big.NewInt(span))
	if err != nil {
		return delay
	}
	jittered := delay - window + time.Duration(offset.Int64())
	if jittered < 0 {
		return 0
	}
	if jittered > maxDelay {
		return maxDelay
	}
	return jittered
}
