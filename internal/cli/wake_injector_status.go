package cli

import (
	"errors"
	"fmt"
	"strings"
)

const (
	wakeInjectorUnsupportedStatus   = "injector_unsupported"
	wakeInputRecoveryRequiredStatus = "input_recovery_required"
	tiocstiLegacySysctlPath         = "/proc/sys/dev/tty/legacy_tiocsti"
	wakeInjectorProgressPrefix      = "AMQ_INJECT_PROGRESS="
)

type wakeInjectorProgress string

const (
	wakeInjectorProgressAccepted  wakeInjectorProgress = "accepted"
	wakeInjectorProgressDeferred  wakeInjectorProgress = "deferred"
	wakeInjectorProgressUncertain wakeInjectorProgress = "uncertain"
)

// parseWakeInjectorProgress accepts only exact complete marker lines. A single
// trailing carriage return is allowed for CRLF output. Uncertain has
// precedence over every other marker so a contradictory provider response can
// never be treated as a successful or replayable delivery.
func parseWakeInjectorProgress(stderr string) wakeInjectorProgress {
	accepted := false
	deferred := false
	uncertain := false
	for _, rawLine := range strings.Split(stderr, "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		switch line {
		case wakeInjectorProgressPrefix + string(wakeInjectorProgressDeferred):
			deferred = true
		case wakeInjectorProgressPrefix + string(wakeInjectorProgressAccepted):
			accepted = true
		case wakeInjectorProgressPrefix + string(wakeInjectorProgressUncertain):
			uncertain = true
		}
	}
	if uncertain {
		return wakeInjectorProgressUncertain
	}
	if accepted && deferred {
		return wakeInjectorProgressUncertain
	}
	if deferred {
		return wakeInjectorProgressDeferred
	}
	if accepted {
		return wakeInjectorProgressAccepted
	}
	return ""
}

type wakeInjectorDeferredError struct {
	err error
}

func (err *wakeInjectorDeferredError) Error() string {
	return fmt.Sprintf("inject-via deferred before provider dispatch: %v", err.err)
}

func (err *wakeInjectorDeferredError) Unwrap() error {
	return err.err
}

func isWakeInjectorDeferred(err error) bool {
	var deferred *wakeInjectorDeferredError
	return errors.As(err, &deferred)
}

type wakeInjectorLegacyError struct {
	err error
}

func (err *wakeInjectorLegacyError) Error() string {
	return fmt.Sprintf("inject-via legacy success is not provider acceptance: %v", err.err)
}

func (err *wakeInjectorLegacyError) Unwrap() error {
	return err.err
}

func isWakeInjectorLegacy(err error) bool {
	var legacy *wakeInjectorLegacyError
	return errors.As(err, &legacy)
}

type wakeInjectorUnsupportedError struct {
	err error
}

func (err *wakeInjectorUnsupportedError) Error() string {
	return fmt.Sprintf("TIOCSTI injector unsupported: %v", err.err)
}

func (err *wakeInjectorUnsupportedError) Unwrap() error {
	return err.err
}

func newWakeInjectorUnsupportedError(err error) error {
	return &wakeInjectorUnsupportedError{err: err}
}

func wakeInjectorUnsupportedReason(mode string, err error) string {
	return fmt.Sprintf(
		"TIOCSTI injection mode %s is unsupported (%v); on Linux check %s; use --inject-via for synthetic input or --inject-mode none for a non-input notifier",
		mode,
		err,
		tiocstiLegacySysctlPath,
	)
}
