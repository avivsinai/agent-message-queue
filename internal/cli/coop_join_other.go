//go:build !darwin && !linux

package cli

import "errors"

// coopJoinCanRun reports that no session can join on this platform.
func coopJoinCanRun() bool { return false }

func runCoopJoin(args []string) error {
	return errors.New("amq coop join is not supported on this platform (requires macOS or Linux)")
}

func runCoopLeave(args []string) error {
	return errors.New("amq coop leave is not supported on this platform (requires macOS or Linux)")
}
