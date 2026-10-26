//go:build !darwin && !linux

package cli

import (
	"errors"
	"time"
)

var startCoopNamedTUIInjector = func(name, cmdName, root, handle string, execStart time.Time) error {
	_, _, _, _, _ = name, cmdName, root, handle, execStart
	return errors.New("coop named TUI inject requires macOS or Linux")
}

func runCoopNamedInject(_ []string) error {
	return errors.New("amq coop named-inject is not supported on this platform (requires macOS or Linux)")
}
