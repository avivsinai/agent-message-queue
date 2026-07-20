//go:build !darwin && !linux

package cli

// Wake itself is unsupported on these platforms. Keep shared metadata helpers
// buildable without pretending to provide the Unix lifecycle serialization.
func withWakeLifecycleGuard(_, _ string, fn func() error) error {
	return fn()
}
