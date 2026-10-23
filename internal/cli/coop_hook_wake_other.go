//go:build !(darwin || linux)

package cli

import "time"

// coopHookWakeBaseline reports no live wake where amq wake does not run, so
// the guard counts every message.
func coopHookWakeBaseline(root, me string) (time.Time, bool) {
	return time.Time{}, false
}
