package selfupgrade

import (
	"errors"
	"fmt"
)

var ErrExecUnsupported = errors.New("self-upgrade exec is unsupported on this platform")

// ExecSupported reports whether this platform has a verified in-place image
// replacement implementation.
func ExecSupported() bool { return execSupportedPlatform() }

// ExecImage verifies the named image again at the exec boundary and then
// replaces the current process with that image. Linux binds the verified file
// descriptor for fexecve via /proc/self/fd; Darwin re-verifies a private stage
// immediately before pathname exec because it has no fexecve primitive.
func ExecImage(candidate ImageEvidence, argv, env []string) error {
	if err := ValidateImageEvidence(candidate); err != nil {
		return fmt.Errorf("self-upgrade candidate evidence is invalid: %w", err)
	}
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("self-upgrade argv is empty")
	}
	return execImagePlatform(candidate, argv, env)
}
