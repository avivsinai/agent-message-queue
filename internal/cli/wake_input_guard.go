package cli

import (
	"errors"
	"runtime"
	"time"
)

// A submit and an observed terminal drain are different evidence. Only the
// latter can qualify a reminder that sends Enter without presenting text.
type wakeInputGuard struct {
	submitTime   func() time.Time
	sample       func() (ttyInputState, error)
	installation func() string
	plainAttempt bool // sampling failed during this attempt; use legacy behavior until its next retry
	lastSubmit   time.Time
	lastOwnRead  time.Time
}

const (
	inputGuardOff       = "off"
	inputGuardPlain     = "plain"
	inputGuardProper    = "proper (claude)"
	inputGuardNeedsHook = "plain (run amq wake install-dialog-hook)"
	inputGuardLinux     = "off (Linux)"
)

type wakeInputDecision uint8

const (
	inputPlain wakeInputDecision = iota
	inputCleanSubmit
	inputCleanOwn
	inputUnknown
	inputStale
)

func (g *wakeInputGuard) status(settings wakeSettings) string {
	if runtime.GOOS == "linux" {
		return inputGuardLinux
	}
	if !settings.inputGuard {
		return inputGuardOff
	}
	if g == nil || !settings.deferWhileInput || settings.inputMaxHold <= 0 {
		return inputGuardPlain
	}
	return g.installation()
}

func wakeInputGuardValue(status string) string {
	switch status {
	case inputGuardProper:
		return status
	case inputGuardOff, inputGuardLinux:
		return inputGuardOff
	}
	return inputGuardPlain
}

var errWakeInputHeld = errors.New("terminal input was read after the doorbell text")

func (cfg *wakeConfig) beginInputAttempt() {
	if cfg.inputGuard != nil {
		cfg.inputGuard.plainAttempt = false
	}
}

// resumeOwedInput serves retained bytes outside a new notification.
func (cfg *wakeConfig) resumeOwedInput(output wakePayload) error {
	cfg.beginInputAttempt()
	_, err := resumeWakeInputDelivery(cfg)
	if errors.Is(err, errWakeInputHeld) {
		return deliverWakeTransientAttention(cfg, output, nil)
	}
	return err
}

func (cfg *wakeConfig) guardedInputEnabled() bool {
	mode := effectiveInjectMode(cfg)
	return cfg.inputGuard != nil && cfg.settings.inputGuard && cfg.settings.deferWhileInput && cfg.settings.inputMaxHold > 0 &&
		cfg.injectVia == "" && (mode == wakeInjectModeRaw || mode == wakeInjectModePaste)
}

// An unrelated, late hook cannot disarm a seat by replacing its stamp.
// A new wake starts with no evidence from the previous target.
func (cfg *wakeConfig) hasCurrentSubmit() bool {
	if !cfg.guardedInputEnabled() {
		return false
	}
	g := cfg.inputGuard
	if g.installation() != inputGuardProper {
		g.lastSubmit = time.Time{}
		return false
	}
	now := cfg.wakeDoorbellNow()
	if at := g.submitTime(); at.After(g.lastSubmit) && !at.After(now) {
		g.lastSubmit = at
	}
	return !g.lastSubmit.IsZero() && !g.lastSubmit.After(now)
}

// A Darwin tty can defer assigning its read time until it is statted.
// Keep the observation current on the wake's existing two-second tick.
func (cfg *wakeConfig) observeArmedInput() {
	if cfg.guardedInputEnabled() && cfg.inputGuard != nil && (cfg.hasCurrentSubmit() || !cfg.inputGuard.lastOwnRead.IsZero()) {
		_, _ = cfg.inputGuard.sample()
	}
}

func (cfg *wakeConfig) inputDecision(owedEnter bool) wakeInputDecision {
	if cfg.inputGuard == nil || cfg.inputGuard.plainAttempt || !cfg.guardedInputEnabled() {
		return inputPlain
	}
	g := cfg.inputGuard
	armed := cfg.hasCurrentSubmit()
	if !owedEnter && !armed && g.lastOwnRead.IsZero() {
		// No fact about this terminal yet: the quiet window decides.
		return inputPlain
	}
	state, err := g.sample()
	if err == nil && state.pendingBytes > 0 {
		// Give the reader one short chance to consume queued bytes.
		rawInjectSleep(rawInjectDrainPollInterval)
		state, err = g.sample()
	}
	now := cfg.wakeDoorbellNow()
	if err != nil || !state.hasLastRead || state.lastRead.IsZero() || state.lastRead.After(now) {
		g.plainAttempt = true
		return inputPlain
	}
	if state.pendingBytes > 0 {
		return inputUnknown
	}
	if owedEnter && g.lastOwnRead.IsZero() {
		// The wake's last write was never seen read, so an Enter could
		// follow bytes the reader has not taken.
		return inputUnknown
	}
	if !g.lastOwnRead.After(now) && !state.lastRead.After(g.lastOwnRead) {
		return inputCleanOwn
	}
	if !owedEnter && armed && !state.lastRead.After(g.lastSubmit) {
		return inputCleanSubmit
	}
	if stale := cfg.settings.inputStaleFor; stale > 0 && now.Sub(state.lastRead) >= stale {
		return inputStale
	}
	return inputUnknown
}

// enterHeld reports whether an Enter that carries no text must not be
// written: terminal input was read since the wake's own last write, so the
// editor can hold text that is not the wake's. Staleness does not release
// such an Enter; it releases only a full doorbell. A seat with no usable
// sample keeps the plain wake's Enter.
func (cfg *wakeConfig) enterHeld() bool {
	switch cfg.inputDecision(true) {
	case inputUnknown, inputStale:
		return true
	}
	return false
}

// An incomplete paste drain supplies no proof for a following Enter.
func (cfg *wakeConfig) samplePasteDrain() {
	_, _, _ = cfg.waitRawDrain(rawInjectCRDrainTimeout, rawInjectDrainPollInterval)
}

func (cfg *wakeConfig) waitRawDrain(timeout, poll time.Duration) (time.Duration, bool, error) {
	if !cfg.guardedInputEnabled() || !cfg.inputDelivery.guardInput || cfg.inputGuard.plainAttempt {
		return waitForRawInputDrained(timeout, poll)
	}
	g := cfg.inputGuard
	samplePending := func() (int, error) {
		state, err := g.sample()
		if err == nil && state.pendingBytes == 0 && state.hasLastRead && !state.lastRead.IsZero() && !state.lastRead.After(cfg.wakeDoorbellNow()) {
			g.lastOwnRead = state.lastRead
		}
		return state.pendingBytes, err
	}
	waited, drained, err := waitForInputQueueDrain(samplePending, time.Now, time.Sleep, timeout, poll)
	if err != nil {
		g.plainAttempt = true
		return waitForRawInputDrained(timeout-waited, poll)
	}
	if drained {
		// The queue can empty before the reader marks the read on return.
		// Keep the first timestamp if input is queued at the settled sample.
		rawInjectSleep(poll)
		_, _ = samplePending()
	}
	return waited, drained, nil
}
