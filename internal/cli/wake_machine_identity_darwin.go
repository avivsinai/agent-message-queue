//go:build darwin

package cli

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// darwinGethostuuid wraps gethostuuid(2). Overridable for tests.
var darwinGethostuuid = func(uuid *[16]byte, timeout *unix.Timespec) syscall.Errno {
	_, _, errno := unix.Syscall(
		//nolint:staticcheck // unix.SYS_GETHOSTUUID is deprecated but x/sys has no gethostuuid wrapper
		unix.SYS_GETHOSTUUID,
		uintptr(unsafe.Pointer(&uuid[0])),
		uintptr(unsafe.Pointer(timeout)),
		0,
	)
	return errno
}

// readWakeMachineIDPlatform returns the hardware platform UUID via
// gethostuuid(2), the kernel's stable identity for this machine (the same
// value as IOPlatformUUID). The timeout must be finite: a zero timespec makes
// the syscall wait indefinitely.
func readWakeMachineIDPlatform() string {
	var uuid [16]byte
	timeout := unix.Timespec{Sec: 3}
	if errno := darwinGethostuuid(&uuid, &timeout); errno != 0 {
		return ""
	}
	return fmt.Sprintf(
		"%08X-%04X-%04X-%04X-%012X",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16],
	)
}
