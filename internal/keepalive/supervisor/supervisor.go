package supervisor

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

const (
	ActionBackoff     = "backoff"
	ActionDeferred    = "deferred"
	ActionDetached    = "detached"
	ActionEnsured     = "ensured"
	ActionStartFailed = "start_failed"

	defaultActiveCheckInterval = 5 * time.Minute
	defaultDetachedBackoffBase = 5 * time.Minute
	defaultDetachedBackoffMax  = time.Hour
	defaultFailureBackoffBase  = time.Minute
	defaultFailureBackoffMax   = 15 * time.Minute
)

type Adapter interface {
	Probe(ctx context.Context, target string) error
}

type WakeRunner interface {
	StartWake(ctx context.Context, req amq.StartWakeRequest) error
}

type Reconciler struct {
	Wake                WakeRunner
	Adapter             Adapter
	Now                 func() time.Time
	BackoffBase         time.Duration
	BackoffMax          time.Duration
	Jitter              func(time.Duration) time.Duration
	InjectVia           string
	WakeTimeout         time.Duration
	ActiveCheckInterval time.Duration
	DetachedBackoffBase time.Duration
	DetachedBackoffMax  time.Duration
}

type Result struct {
	Action     string
	AMQTouched bool
	Error      error
}

func (r Reconciler) Reconcile(ctx context.Context, entry registry.Entry) (registry.Entry, Result) {
	now := r.now()

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

	if blocked, result, ok := r.checkLocalReadiness(ctx, entry, now); ok {
		return blocked, result
	}

	return r.ensureWake(ctx, entry, now)
}

func (r Reconciler) checkLocalReadiness(ctx context.Context, entry registry.Entry, now time.Time) (registry.Entry, Result, bool) {
	original := entry
	if err := ctx.Err(); err != nil {
		return entry, Result{Action: ActionDeferred, Error: err}, true
	}
	if !entry.NextHealthCheck.IsZero() && now.Before(entry.NextHealthCheck) {
		return entry, Result{Action: ActionDeferred}, true
	}
	if !entry.BackoffUntil.IsZero() && now.Before(entry.BackoffUntil) {
		return entry, Result{Action: ActionDeferred}, true
	}
	entry.LastSeenBySupervisor = now

	if r.Adapter == nil {
		updated, result := r.markBackoff(entry, now, errors.New("adapter is not configured"), ActionBackoff, false)
		return updated, result, true
	}
	if err := r.Adapter.Probe(ctx, entry.Target); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return entry, Result{Action: ActionDeferred, Error: ctxErr}, true
		}
		if errors.Is(err, adapter.ErrTargetNotFound) {
			updated, result := r.markDetached(entry, now, err)
			return updated, result, true
		}
		if errors.Is(err, adapter.ErrTargetDegraded) {
			return original, Result{Action: ActionDeferred, Error: err}, true
		}
		updated, result := r.markBackoff(entry, now, err, ActionBackoff, false)
		return updated, result, true
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
		return r.markActive(entry, now, ActionEnsured), Result{Action: ActionEnsured, AMQTouched: true}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return entry, Result{Action: ActionDeferred, AMQTouched: true, Error: errors.Join(ctxErr, err)}
	}
	return r.markBackoff(entry, now, err, ActionStartFailed, true)
}

func (r Reconciler) markBackoff(entry registry.Entry, now time.Time, err error, action string, amqTouched bool) (registry.Entry, Result) {
	entry.State = registry.StateAttached
	entry.FailureCount++
	entry.BackoffUntil = now.Add(r.backoff(entry.FailureCount))
	entry.NextHealthCheck = time.Time{}
	entry.DetachedSince = time.Time{}
	entry.LastSupervisorDecision = action
	if err != nil {
		entry.LastError = err.Error()
	}
	return entry, Result{Action: action, AMQTouched: amqTouched, Error: err}
}

func (r Reconciler) markDetached(entry registry.Entry, now time.Time, err error) (registry.Entry, Result) {
	entry.State = registry.StateDetached
	entry.FailureCount++
	entry.BackoffUntil = now.Add(r.detachedBackoff(entry.FailureCount))
	entry.NextHealthCheck = time.Time{}
	if entry.DetachedSince.IsZero() {
		entry.DetachedSince = now
	}
	entry.LastError = err.Error()
	entry.LastSupervisorDecision = ActionDetached
	return entry, Result{Action: ActionDetached, Error: err}
}

func (r Reconciler) markActive(entry registry.Entry, now time.Time, action string) registry.Entry {
	entry.State = registry.StateActive
	entry.LastSeenBySupervisor = now
	entry.FailureCount = 0
	entry.BackoffUntil = time.Time{}
	entry.DetachedSince = time.Time{}
	entry.NextHealthCheck = now.Add(r.activeCheckInterval())
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
		base = defaultFailureBackoffBase
	}
	maxDelay := r.BackoffMax
	if maxDelay <= 0 {
		maxDelay = defaultFailureBackoffMax
	}
	return r.exponentialBackoff(failureCount, base, maxDelay)
}

func (r Reconciler) detachedBackoff(failureCount int) time.Duration {
	base := r.DetachedBackoffBase
	if base <= 0 {
		base = defaultDetachedBackoffBase
	}
	maxDelay := r.DetachedBackoffMax
	if maxDelay <= 0 {
		maxDelay = defaultDetachedBackoffMax
	}
	return r.exponentialBackoff(failureCount, base, maxDelay)
}

func (r Reconciler) exponentialBackoff(failureCount int, base, maxDelay time.Duration) time.Duration {
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

func (r Reconciler) activeCheckInterval() time.Duration {
	if r.ActiveCheckInterval > 0 {
		return r.ActiveCheckInterval
	}
	return defaultActiveCheckInterval
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
