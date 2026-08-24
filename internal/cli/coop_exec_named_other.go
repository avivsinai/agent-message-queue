//go:build !darwin && !linux

package cli

import "errors"

var startCoopNamedTUIInjector = func(me, cmdName string) error {
	_ = me
	_ = cmdName
	return errors.New("coop named TUI inject requires macOS or Linux")
}

func runCoopNamedInject(_ []string) error {
	return errors.New("amq coop named-inject is not supported on this platform (requires macOS or Linux)")
}
