//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package notificationattempt

import (
	"errors"
	"os"
)

// LedgerSupported reports whether this platform provides the flock primitive
// required to coordinate journal rotation.
const LedgerSupported = false

// errLockingUnsupported is returned by the no-op fallbacks on platforms
// without syscall.Flock. The ledger still compiles and runs; the rotation
// race is not closed there, but those platforms (e.g. wasm/plan9) are not
// wake hosts. This mirrors the keepalive registry's cross-process-lock
// stance: fail closed on the guarantee rather than silently weaken it.
var errLockingUnsupported = errors.New("notification attempt journal locking is unsupported on this platform")

func flockShared(_ *os.File) error    { return errLockingUnsupported }
func flockExclusive(_ *os.File) error { return errLockingUnsupported }
func flockRelease(_ *os.File)         {}
