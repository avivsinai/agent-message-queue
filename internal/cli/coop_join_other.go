//go:build !darwin && !linux

package cli

import "errors"

func runCoopJoin(args []string) error {
	return errors.New("amq coop join is not supported on this platform (requires macOS or Linux)")
}

func runCoopLeave(args []string) error {
	return errors.New("amq coop leave is not supported on this platform (requires macOS or Linux)")
}
