package cli

import (
	"os"
	"time"
)

type wakeDoorbellPhase uint8

const (
	wakeDoorbellIdle wakeDoorbellPhase = iota
	wakeDoorbellRetrying
	wakeDoorbellParked
	wakeDoorbellAnnounced
	wakeDoorbellRecoveryRequired
)

const (
	wakeDoorbellRetryBase          = 5 * time.Second
	wakeDoorbellAttentionRetryBase = 30 * time.Second
	wakeDoorbellRetryMax           = 2 * time.Minute
	wakeDoorbellAttentionRetryMax  = 15 * time.Minute

	// Repeated synthetic input is not consumer-safe: queueing consumers may
	// retain every identical reminder and flush them as one later turn. Bound
	// an unchanged cohort, while leaving additions a small finite chance to
	// announce genuinely new work.
	wakeDoorbellInitialAttemptBudget = uint(4)
	wakeDoorbellLifetimeAttemptCap   = uint(8)
)

type wakeDoorbellState struct {
	phase                        wakeDoorbellPhase
	cohort                       map[string]*wakeFileIdentity
	presentationConfirmed        bool
	attempts                     uint
	reminderAttempts             uint
	attemptBudget                uint
	nextAttempt                  time.Time
	additionAttemptFloor         time.Time
	transientAttentionAttempts   uint
	nextTransientAttention       time.Time
	recoveryPending              bool
	recoveryAttentionUndelivered bool
}

type wakeDoorbellPlan struct {
	attempt       bool
	prompt        string
	submitOnly    bool
	contentChange bool
	progress      bool
}

func (state *wakeDoorbellState) plan(
	now time.Time,
	current map[string]os.FileInfo,
) wakeDoorbellPlan {
	if len(current) == 0 {
		state.reset()
		return wakeDoorbellPlan{}
	}
	if state.phase == wakeDoorbellAnnounced {
		// A replaced physical file keeps its name but is a message the agent
		// has never seen; it re-arms like an addition, not like a drain.
		if wakeCohortExpanded(state.cohort, current) ||
			wakeCohortReplacedInPlace(state.cohort, current) {
			state.arm(current)
			return wakeDoorbellPlan{attempt: true, prompt: coopWakeDoorbell}
		}
		if wakeCohortProgressed(state.cohort, current) {
			state.recordInjected(current)
		}
		return wakeDoorbellPlan{}
	}

	progress := state.phase != wakeDoorbellIdle && wakeCohortProgressed(state.cohort, current)
	contentChange := false
	if progress {
		state.reset()
	} else if state.phase != wakeDoorbellIdle && wakeCohortExpanded(state.cohort, current) {
		contentChange = true
		// Additions extend the pending obligation without resetting its retry
		// ladder, but pull its deadline forward to the delivery floor because
		// the new information has not been announced yet. One outstanding
		// "drain everything" doorbell covers the whole current cohort, so N
		// unread messages do not need N doorbells.
		if state.phase == wakeDoorbellParked {
			state.cohort = snapshotWakeFileIdentities(current)
			state.presentationConfirmed = false
			if state.attemptBudget < wakeDoorbellLifetimeAttemptCap {
				state.attemptBudget++
				state.phase = wakeDoorbellRetrying
				state.pullForwardForAddition(now)
			}
		} else {
			state.arm(current)
			state.pullForwardForAddition(now)
		}
	}
	if state.phase == wakeDoorbellIdle {
		state.arm(current)
	}
	if state.phase == wakeDoorbellParked {
		return wakeDoorbellPlan{}
	}
	if state.attempts > 0 && now.Before(state.nextAttempt) {
		return wakeDoorbellPlan{}
	}

	return wakeDoorbellPlan{
		attempt:       true,
		prompt:        coopWakeDoorbell,
		submitOnly:    state.presentationConfirmed,
		contentChange: contentChange,
		progress:      progress,
	}
}

func (state *wakeDoorbellState) arm(current map[string]os.FileInfo) {
	state.phase = wakeDoorbellRetrying
	state.cohort = snapshotWakeFileIdentities(current)
	state.presentationConfirmed = false
	if state.attemptBudget == 0 {
		state.attemptBudget = wakeDoorbellInitialAttemptBudget
	}
}

func (state *wakeDoorbellState) confirmPresentation() {
	state.presentationConfirmed = true
}

func (state *wakeDoorbellState) recordAttempt(now time.Time) {
	if state.attemptBudget == 0 {
		state.attemptBudget = wakeDoorbellInitialAttemptBudget
	}
	state.recordAttemptWithBase(now, wakeDoorbellRetryBase, wakeDoorbellRetryMax)
	state.reminderAttempts++
	if state.reminderAttempts >= state.attemptBudget {
		state.parkCurrentCohort()
	}
}

func (state *wakeDoorbellState) recordDeferredInputAttempt(now time.Time) {
	state.recordAttemptWithBase(now, wakeDoorbellRetryBase, wakeDoorbellRetryMax)
}

// parkCurrentCohort is the acknowledgement seam: a future injected-ack policy
// can park the acknowledged cohort here without changing the budget model.
func (state *wakeDoorbellState) parkCurrentCohort() {
	state.phase = wakeDoorbellParked
	state.nextAttempt = time.Time{}
}

func (state *wakeDoorbellState) recordInjected(current map[string]os.FileInfo) {
	state.reset()
	state.phase = wakeDoorbellAnnounced
	state.cohort = snapshotWakeFileIdentities(current)
}

func (state *wakeDoorbellState) recordAttentionAttempt(now time.Time) {
	state.recordAttemptWithBase(
		now,
		wakeDoorbellAttentionRetryBase,
		wakeDoorbellAttentionRetryMax,
	)
	// Additions join the continuously unread cohort and are rendered at its
	// existing attention deadline. Pulling every decayed retry back to the
	// 30-second floor lets a steady arrival stream defeat the attention ladder.
	state.additionAttemptFloor = time.Time{}
}

func (state *wakeDoorbellState) transientAttentionDue(now time.Time) bool {
	return state.nextTransientAttention.IsZero() ||
		!now.Before(state.nextTransientAttention)
}

func (state *wakeDoorbellState) recordTransientAttentionAttempt(now time.Time) {
	state.transientAttentionAttempts++
	state.nextTransientAttention = now.Add(cappedExponentialBackoff(
		state.transientAttentionAttempts,
		wakeDoorbellAttentionRetryBase,
		wakeDoorbellAttentionRetryMax,
	))
}

func (state *wakeDoorbellState) recordAttemptWithBase(
	now time.Time,
	base, maximum time.Duration,
) {
	state.attempts++
	state.additionAttemptFloor = now.Add(base)
	state.nextAttempt = now.Add(cappedExponentialBackoff(
		state.attempts,
		base,
		maximum,
	))
}

func (state *wakeDoorbellState) pullForwardForAddition(now time.Time) {
	if state.additionAttemptFloor.IsZero() {
		return
	}
	deadline := state.additionAttemptFloor
	if deadline.Before(now) {
		deadline = now
	}
	if state.nextAttempt.IsZero() || deadline.Before(state.nextAttempt) {
		state.nextAttempt = deadline
	}
}

func (state *wakeDoorbellState) planRecoveryAttention(
	now time.Time,
	current map[string]os.FileInfo,
) bool {
	if len(current) == 0 {
		if state.phase == wakeDoorbellRecoveryRequired &&
			state.recoveryAttentionUndelivered {
			return state.nextAttempt.IsZero() ||
				!now.Before(state.nextAttempt)
		}
		state.noteRecoveryInboxEmpty()
		return false
	}
	if state.phase != wakeDoorbellRecoveryRequired {
		return true
	}
	state.recoveryPending = true
	if !state.nextAttempt.IsZero() && now.Before(state.nextAttempt) {
		return false
	}
	return true
}

func (state *wakeDoorbellState) recordRecoveryRequired(
	now time.Time,
	current map[string]os.FileInfo,
) {
	if state.phase != wakeDoorbellRecoveryRequired {
		state.reset()
	}
	state.phase = wakeDoorbellRecoveryRequired
	state.cohort = snapshotWakeFileIdentities(current)
	state.recoveryPending = true
	state.recoveryAttentionUndelivered = true
	state.recordAttentionAttempt(now)
}

func (state *wakeDoorbellState) retainRecoveryRequired(now time.Time) {
	cohort := state.cohort
	state.reset()
	state.phase = wakeDoorbellRecoveryRequired
	state.cohort = cohort
	state.recoveryPending = true
	state.recoveryAttentionUndelivered = true
	state.recordAttentionAttempt(now)
}

func (state *wakeDoorbellState) recordRecoveryAttentionDelivered() {
	state.recoveryAttentionUndelivered = false
}

func (state *wakeDoorbellState) noteRecoveryInboxEmpty() {
	state.reset()
}

func (state *wakeDoorbellState) makeDue(now time.Time) {
	if state.phase != wakeDoorbellRetrying || len(state.cohort) == 0 {
		return
	}
	state.nextAttempt = now
}

func (state wakeDoorbellState) pendingInput() bool {
	return state.phase == wakeDoorbellRetrying
}

func (state wakeDoorbellState) parkedReminderAttempts() (uint, bool) {
	return state.reminderAttempts, state.phase == wakeDoorbellParked
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

func wakeCohortExpanded(
	cohort map[string]*wakeFileIdentity,
	current map[string]os.FileInfo,
) bool {
	for name := range current {
		if _, exists := cohort[name]; !exists {
			return true
		}
	}
	return false
}

// wakeCohortReplacedInPlace reports whether a cohort member's name now refers
// to a provably different physical file. Unknown identities on either side
// stay conservative and report no replacement.
func wakeCohortReplacedInPlace(
	cohort map[string]*wakeFileIdentity,
	current map[string]os.FileInfo,
) bool {
	for name, identity := range cohort {
		info, ok := current[name]
		if !ok || info == nil || identity == nil {
			continue
		}
		currentIdentity, known := captureWakeFileIdentity(info)
		if !known {
			continue
		}
		if *identity != currentIdentity {
			return true
		}
	}
	return false
}
