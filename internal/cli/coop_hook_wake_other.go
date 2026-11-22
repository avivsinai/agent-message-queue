//go:build !(darwin || linux)

package cli

import "time"

// coopHookWakeBaseline reports no live wake where amq wake does not run, so
// the guard counts every message.
func coopHookWakeBaseline(root, me string) (time.Time, bool) {
	return time.Time{}, false
}

// On this platform no wake or wait hook runs.
func readSeatDoorbell(root, me string) seatDoorbellRead {
	return seatDoorbellRead{ringer: doorbellHook}
}
