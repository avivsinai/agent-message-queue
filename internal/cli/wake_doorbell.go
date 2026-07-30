package cli

import (
	"os"
	"time"
)

type wakeDoorbellPhase uint8

const (
	wakeDoorbellIdle wakeDoorbellPhase = iota
	wakeDoorbellRetrying
	wakeDoorbellRecoveryRequired
)

const (
	wakeDoorbellRetryBase          = 5 * time.Second
	wakeDoorbellAttentionRetryBase = 30 * time.Second
	wakeDoorbellRetryMax           = 2 * time.Minute
	wakeDoorbellAttentionRetryMax  = 15 * time.Minute
)

type wakeDoorbellState struct {
	phase           wakeDoorbellPhase
	cohort          map[string]*wakeFileIdentity
	attempts        uint
	nextAttempt     time.Time
	recoveryPending bool
}

type wakeDoorbellPlan struct {
	attempt  bool
	prompt   string
	progress bool
}

func (state *wakeDoorbellState) plan(
	now time.Time,
	current map[string]os.FileInfo,
) wakeDoorbellPlan {
	if len(current) == 0 {
		state.reset()
		return wakeDoorbellPlan{}
	}

	progress := state.phase != wakeDoorbellIdle && wakeCohortProgressed(state.cohort, current)
	if progress {
		state.reset()
	}
	if state.phase == wakeDoorbellIdle {
		state.arm(current)
	}
	if state.attempts > 0 && now.Before(state.nextAttempt) {
		return wakeDoorbellPlan{}
	}

	return wakeDoorbellPlan{
		attempt:  true,
		prompt:   coopWakeDoorbell,
		progress: progress,
	}
}

func (state *wakeDoorbellState) arm(current map[string]os.FileInfo) {
	state.phase = wakeDoorbellRetrying
	state.cohort = snapshotWakeFileIdentities(current)
}

func (state *wakeDoorbellState) recordAttempt(now time.Time) {
	state.recordAttemptWithBase(now, wakeDoorbellRetryBase, wakeDoorbellRetryMax)
}

func (state *wakeDoorbellState) recordAttentionAttempt(now time.Time) {
	state.recordAttemptWithBase(
		now,
		wakeDoorbellAttentionRetryBase,
		wakeDoorbellAttentionRetryMax,
	)
}

func (state *wakeDoorbellState) recordAttemptWithBase(
	now time.Time,
	base, maximum time.Duration,
) {
	state.attempts++
	state.nextAttempt = now.Add(cappedExponentialBackoff(
		state.attempts,
		base,
		maximum,
	))
}

func (state *wakeDoorbellState) planRecoveryAttention(
	now time.Time,
	current map[string]os.FileInfo,
) bool {
	if len(current) == 0 {
		state.noteRecoveryInboxEmpty()
		return false
	}
	if state.phase != wakeDoorbellRecoveryRequired {
		return true
	}
	if sameKnownWakeCohort(state.cohort, current) {
		state.recoveryPending = false
		return false
	}
	if !state.nextAttempt.IsZero() && now.Before(state.nextAttempt) {
		state.recoveryPending = true
		return false
	}
	return true
}

func (state *wakeDoorbellState) recordRecoveryRequired(
	now time.Time,
	current map[string]os.FileInfo,
) {
	state.reset()
	state.phase = wakeDoorbellRecoveryRequired
	state.cohort = snapshotWakeFileIdentities(current)
	state.nextAttempt = now.Add(wakeDoorbellRetryBase)
}

func (state *wakeDoorbellState) retainRecoveryRequired(now time.Time) {
	cohort := state.cohort
	state.reset()
	state.phase = wakeDoorbellRecoveryRequired
	state.cohort = cohort
	state.nextAttempt = now.Add(wakeDoorbellRetryBase)
}

func (state *wakeDoorbellState) noteRecoveryInboxEmpty() {
	if state.phase != wakeDoorbellRecoveryRequired {
		state.reset()
		state.phase = wakeDoorbellRecoveryRequired
	}
	state.recoveryPending = false
}

func (state wakeDoorbellState) pendingInput() bool {
	return state.phase == wakeDoorbellRetrying
}

func (state *wakeDoorbellState) nextDeadline() (time.Time, bool) {
	switch state.phase {
	case wakeDoorbellRetrying:
		return state.nextAttempt, !state.nextAttempt.IsZero()
	case wakeDoorbellRecoveryRequired:
		return state.nextAttempt,
			state.recoveryPending && !state.nextAttempt.IsZero()
	default:
		return time.Time{}, false
	}
}

func (state *wakeDoorbellState) reset() {
	*state = wakeDoorbellState{}
}

func cappedExponentialBackoff(attempt uint, base, maximum time.Duration) time.Duration {
	delay := base
	if delay >= maximum {
		return maximum
	}
	for i := uint(1); i < attempt && delay < maximum; i++ {
		delay *= 2
		if delay >= maximum {
			return maximum
		}
	}
	return delay
}

func wakeCohortProgressed(
	cohort map[string]*wakeFileIdentity,
	current map[string]os.FileInfo,
) bool {
	for name, identity := range cohort {
		info, ok := current[name]
		if !ok {
			return true
		}
		if info == nil {
			continue
		}
		currentIdentity, known := captureWakeFileIdentity(info)
		if identity == nil {
			if known {
				return true
			}
			continue
		}
		if !known {
			continue
		}
		if *identity != currentIdentity {
			return true
		}
	}
	return false
}
