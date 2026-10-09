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
	// holdUntil delays a new announcement, including an addition that revives
	// a parked cohort. It is separate from the delivery retry deadline.
	holdUntil time.Time
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
	return state.planHeld(now, current, nil)
}

// planHeld is plan with hold-by-priority. holds maps an inbox file name to
// its hold; nil means no holds, which is exactly plan().
func (state *wakeDoorbellState) planHeld(
	now time.Time,
	current map[string]os.FileInfo,
	holds map[string]wakeHold,
) wakeDoorbellPlan {
	if len(current) == 0 {
		state.reset()
		return wakeDoorbellPlan{}
	}
	if state.phase == wakeDoorbellAnnounced {
		previous := state.cohort
		if state.reconcileAnnouncedCohort(current) {
			state.holdUntil = earliestWakeHold(holds, current, previous)
			if state.holdActive(now) {
				return wakeDoorbellPlan{}
			}
			state.holdUntil = time.Time{}
			return wakeDoorbellPlan{attempt: true, prompt: coopWakeDoorbell}
		}
		return wakeDoorbellPlan{}
	}

	progress := state.phase != wakeDoorbellIdle && wakeCohortProgressed(state.cohort, current)
	contentChange := false
	if progress {
		state.reset()
	} else if state.phase != wakeDoorbellIdle && wakeCohortExpanded(state.cohort, current) {
		contentChange = true
		added := earliestWakeHold(holds, current, state.cohort)
		urgent := wakeAdditionUrgent(holds, current, state.cohort)
		// Additions extend the pending obligation without resetting its retry
		// ladder, but pull its deadline forward to the delivery floor because
		// the new information has not been announced yet. One outstanding
		// "drain everything" doorbell covers the whole current cohort, so N
		// unread messages do not need N doorbells.
		//
		// Hold-by-priority: before the first doorbell an addition may only
		// pull the hold deadline earlier. After a doorbell, a held addition
		// rides on the retry ladder; only urgent mail (or mail whose hold is
		// over, or no hold at all) pulls the ladder forward. Urgent mail
		// also revives a parked cohort past the attempt budget.
		switch {
		case state.phase == wakeDoorbellParked && urgent:
			state.reviveParkedCohortForUrgent(current)
			state.nextAttempt = now
		case state.phase == wakeDoorbellParked:
			if state.reviveParkedCohort(current) {
				if state.holdsAddition(added, now) {
					state.holdUntil = added
				} else {
					state.pullForwardForAddition(now)
				}
			}
		default:
			state.arm(current)
			switch {
			case !state.holdUntil.IsZero():
				if !added.IsZero() && added.Before(state.holdUntil) {
					state.holdUntil = added
				}
			case urgent:
				state.nextAttempt = now
			case !state.holdsAddition(added, now):
				state.pullForwardForAddition(now)
			}
		}
	}
	if state.phase == wakeDoorbellIdle {
		state.arm(current)
		state.holdUntil = earliestWakeHold(holds, current, nil)
	}
	if state.phase == wakeDoorbellParked {
		return wakeDoorbellPlan{}
	}
	if state.holdActive(now) {
		return wakeDoorbellPlan{}
	}
	state.holdUntil = time.Time{}
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

// holdActive reports whether hold-by-priority delays a new announcement.
func (state *wakeDoorbellState) holdActive(now time.Time) bool {
	return !state.holdUntil.IsZero() && now.Before(state.holdUntil)
}

// holdsAddition reports whether an addition's hold (zero = none) has not
// elapsed, so the addition should wait for the existing retry ladder.
func (state *wakeDoorbellState) holdsAddition(added, now time.Time) bool {
	return !added.IsZero() && now.Before(added)
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

// reconcileAnnouncedCohort applies the announced-phase cohort rules shared by
// plan() and the deferred interrupt path. A replaced physical file keeps its
// name but is a message the agent has never seen; it re-arms like an
// addition, not like a drain. Progress alone re-snapshots the cohort to what
// remains. It reports whether the ladder was re-armed.
func (state *wakeDoorbellState) reconcileAnnouncedCohort(current map[string]os.FileInfo) bool {
	if wakeCohortExpanded(state.cohort, current) ||
		wakeCohortReplacedInPlace(state.cohort, current) {
		state.arm(current)
		return true
	}
	if wakeCohortProgressed(state.cohort, current) {
		state.recordInjected(current)
	}
	return false
}

// reviveParkedCohort applies the parked-phase addition rule shared by plan()
// and the deferred interrupt path: the parked cohort adopts the expanded
// content, and the ladder revives only while the lifetime attempt budget
// allows another announcement. It reports whether the ladder revived.
func (state *wakeDoorbellState) reviveParkedCohort(current map[string]os.FileInfo) bool {
	state.cohort = snapshotWakeFileIdentities(current)
	state.presentationConfirmed = false
	if state.attemptBudget >= wakeDoorbellLifetimeAttemptCap {
		return false
	}
	state.attemptBudget++
	state.phase = wakeDoorbellRetrying
	return true
}

// reviveParkedCohortForUrgent revives a parked cohort for new urgent mail
// regardless of the lifetime attempt cap, with exactly one more attempt. The
// urgent message joins the cohort snapshot, so seeing it again is not an
// addition and cannot revive the cohort again.
func (state *wakeDoorbellState) reviveParkedCohortForUrgent(current map[string]os.FileInfo) {
	state.cohort = snapshotWakeFileIdentities(current)
	state.presentationConfirmed = false
	state.attemptBudget = state.reminderAttempts + 1
	state.phase = wakeDoorbellRetrying
}

// reconcileDeferredCohort applies plan()'s announced/parked cohort rules for
// attempts recorded outside a plan() pass — the interrupt path injects before
// the doorbell plan runs. A deferred provider outcome must leave the retry
// ladder armed for cohort content the agent has not seen; without this, a
// deferred attempt recorded in the announced or parked phase writes into
// state nextDeadline() never reads and the doorbell stalls until an unrelated
// inbox event. The caller advances the retry deadline afterwards, so no
// pull-forward happens here.
func (state *wakeDoorbellState) reconcileDeferredCohort(current map[string]os.FileInfo, holds map[string]wakeHold) {
	if len(current) == 0 {
		return
	}
	switch state.phase {
	case wakeDoorbellAnnounced:
		state.reconcileAnnouncedCohort(current)
	case wakeDoorbellParked:
		if wakeCohortProgressed(state.cohort, current) {
			// plan(): progress on a parked cohort (a drain or an in-place
			// replacement) resets the ladder and arms the remaining cohort
			// fresh.
			state.reset()
			state.arm(current)
			return
		}
		if wakeCohortExpanded(state.cohort, current) {
			if wakeAdditionUrgent(holds, current, state.cohort) {
				state.reviveParkedCohortForUrgent(current)
			} else {
				state.reviveParkedCohort(current)
			}
		}
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
	// The interrupt path can attempt delivery before planHeld consumes a hold.
	// Once an attempt occurs, only the delivery retry deadline applies.
	state.holdUntil = time.Time{}
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
		if !state.holdUntil.IsZero() {
			return state.holdUntil, true
		}
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
