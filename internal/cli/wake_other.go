//go:build !darwin && !linux

package cli

import "errors"

func runWake(args []string) error {
	if len(args) > 0 && args[0] == "check" && wakeCheckV2OptInPresent(args[1:]) {
		return runWakeCheckUnsupported(args[1:])
	}
	return errors.New("amq wake is not supported on this platform (requires macOS or Linux)")
}
