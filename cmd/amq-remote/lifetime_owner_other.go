//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package main

// lifetimeOwnerPidOS reports "unknown" on platforms without a discoverable
// lock owner (N3): the refusal still goes out, naming the entry and
// registry, with pid 0 meaning unknown. Fail-closed never fakes a verdict.
// (The lifetime flock itself is refused unconditionally on these platforms —
// up_flock_other.go — so this is only reachable in the refusal's text.)
func lifetimeOwnerPidOS(regPath, entryID string) int {
	return 0
}
