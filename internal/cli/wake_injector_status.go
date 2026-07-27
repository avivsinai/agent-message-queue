package cli

import (
	"fmt"
)

const (
	wakeInjectorUnsupportedStatus = "injector_unsupported"
	wakeInjectorUnverifiedStatus  = "injector_unverified"
	tiocstiLegacySysctlPath       = "/proc/sys/dev/tty/legacy_tiocsti"
)

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

func wakeInjectorUnverifiedReason(mode, verified string) string {
	return fmt.Sprintf(
		"TIOCSTI injection mode %s is unverified: %s; no TIOCSTI test injection was attempted",
		mode,
		verified,
	)
}
