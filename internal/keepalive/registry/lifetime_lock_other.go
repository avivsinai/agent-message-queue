//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package registry

import "errors"

// ProbeLifetimeLock on platforms without advisory flock (611.13.2 review):
// holding cannot be determined, so the probe reports an ERROR. Callers must
// fail closed - up never reclaims a phantom there and doctor --ops reports
// the row as unknown - never a silent not-held, which would reclaim a row
// that might have a living supervisor.
func ProbeLifetimeLock(regPath, entryID string) (bool, error) {
	return false, errors.Join(errLockProbeFailed, errors.New("lifetime lock probe unavailable on this platform"))
}
