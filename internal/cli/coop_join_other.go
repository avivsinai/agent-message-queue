//go:build !darwin && !linux

package cli

import "errors"

// coopJoinCanRun reports that no session can join on this platform.
func coopJoinCanRun() bool { return false }

// claudeKeyOf reads no session record on this platform, so the key of a
// session is its Claude session id.
func claudeKeyOf(configDir, claudeSession string) string { return claudeSession }

func runCoopJoin(args []string) error {
	return errors.New("amq coop join is not supported on this platform (requires macOS or Linux)")
}

func runCoopLeave(args []string) error {
	return errors.New("amq coop leave is not supported on this platform (requires macOS or Linux)")
}
