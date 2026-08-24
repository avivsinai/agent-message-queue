//go:build !darwin && !linux

package cli

import "errors"

func startCoopNamedTUIInjector(me, cmdName string) error {
	_ = me
	_ = cmdName
	return nil
}

func runCoopNamedInject(_ []string) error {
	return errors.New("amq coop named-inject is not supported on this platform (requires macOS or Linux)")
}
