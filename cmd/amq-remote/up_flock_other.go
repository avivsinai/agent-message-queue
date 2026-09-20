//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package main

import (
	"os"
)

// flockLifetime fail-closes on platforms without advisory flock: single-
// owner enforcement is unavailable there, so every up on such a platform is
// refused (errLifetimeHeld) rather than silently allowing two supervisors on
// one root (registry writes still serialize). Swarm interop targets
// macOS/Linux; keep the other builds compiling.
func flockLifetime(*os.File) error {
	return errLifetimeHeld
}
