//go:build !darwin && !linux

package cli

import (
	"errors"
	"time"
)

var startCoopNamedTUIInjector = func(name, cmdName string, execStart time.Time) error {
	_ = name
	_ = cmdName
	_ = execStart
	return errors.New("coop named TUI inject requires macOS or Linux")
}

func runCoopNamedInject(_ []string) error {
	return errors.New("amq coop named-inject is not supported on this platform (requires macOS or Linux)")
}
