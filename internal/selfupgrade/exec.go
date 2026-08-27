package selfupgrade

import (
	"errors"
	"fmt"
)

var ErrExecUnsupported = errors.New("self-upgrade exec is unsupported on this platform")

// ExecImage verifies the named image again at the exec boundary and then
// replaces the current process with that image. The platform implementations
// bind the verified bytes before calling execve where the host provides a
// suitable primitive.
func ExecImage(candidate ImageEvidence, argv, env []string) error {
	if err := ValidateImageEvidence(candidate); err != nil {
		return fmt.Errorf("self-upgrade candidate evidence is invalid: %w", err)
	}
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("self-upgrade argv is empty")
	}
	return execImagePlatform(candidate, argv, env)
}
