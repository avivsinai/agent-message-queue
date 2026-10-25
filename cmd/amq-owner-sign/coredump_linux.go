//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// forbidCoreDumps makes this process undumpable, so no crash, whatever
// GOTRACEBACK says, writes its memory and the owner key to disk. The zero
// core limit stops a core file; it does not stop a core_pattern that pipes
// the dump to a handler such as systemd-coredump or Apport, which the kernel
// runs regardless of RLIMIT_CORE. PR_SET_DUMPABLE 0 stops every dump,
// including a piped one. Both settings only lower what the process may do,
// so they need no privilege.
func forbidCoreDumps() error {
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		return fmt.Errorf("set RLIMIT_CORE: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl PR_SET_DUMPABLE: %w", err)
	}
	return nil
}
