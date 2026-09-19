//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package main

import (
	"os"
)

// flockLifetime is a best-effort no-op on platforms without advisory flock:
// single-owner enforcement is unavailable there, so two ups on one root are
// not refused (registry writes still serialize). Swarm interop targets
// macOS/Linux; keep the other builds compiling.
func flockLifetime(*os.File) error {
	return errLifetimeHeld
}
