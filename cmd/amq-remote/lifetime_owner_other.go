//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package main

// lifetimeOwnerPidOS reports "unknown" on platforms without a discoverable
// lock owner (N3): the refusal still goes out, naming the entry and
// registry, with pid 0 meaning unknown. Fail-closed never fakes a verdict.
func lifetimeOwnerPidOS(regPath, entryID string) int {
	return 0
}
