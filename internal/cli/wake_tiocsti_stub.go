//go:build !darwin && !linux

package cli

import (
	"errors"
	"time"
)

// tiocsti provides a stub for non-Unix systems.
var tiocsti = tiocstiFuncs{}

type tiocstiFuncs struct{}

// Available returns false on non-Unix systems.
func (t tiocstiFuncs) Available() bool {
	return false
}

// IsTTY returns false on non-Unix systems.
func (t tiocstiFuncs) IsTTY() bool {
	return false
}

// Inject returns an error on non-Unix systems.
func (t tiocstiFuncs) Inject(text string) error {
	return errors.New("TIOCSTI not available on this platform")
}

func waitForTTYInputQuiet(cfg *wakeConfig) {}

func waitForTTYInputDrain(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
	return 0, false, errors.New("TTY input drain unavailable on this platform")
}
