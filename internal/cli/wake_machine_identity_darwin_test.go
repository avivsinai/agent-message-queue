//go:build darwin

package cli

import (
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func stubDarwinGethostuuid(t *testing.T, fn func(uuid *[16]byte, timeout *unix.Timespec) syscall.Errno) {
	t.Helper()
	old := darwinGethostuuid
	darwinGethostuuid = fn
	t.Cleanup(func() { darwinGethostuuid = old })
}

func TestReadWakeMachineIDPlatformFormatsUUIDAndPassesFiniteTimeout(t *testing.T) {
	var captured unix.Timespec
	stubDarwinGethostuuid(t, func(uuid *[16]byte, timeout *unix.Timespec) syscall.Errno {
		captured = *timeout
		*uuid = [16]byte{
			0x10, 0x28, 0x4D, 0xF2, 0x40, 0xDE, 0x50, 0xF6,
			0xBC, 0x1C, 0x4E, 0x79, 0xBA, 0xE6, 0x99, 0xE9,
		}
		return 0
	})
	if got := readWakeMachineIDPlatform(); got != "10284DF2-40DE-50F6-BC1C-4E79BAE699E9" {
		t.Fatalf("machine id = %q, want formatted platform UUID", got)
	}
	if captured.Sec <= 0 && captured.Nsec <= 0 {
		t.Fatalf("gethostuuid timeout = %+v, want finite non-zero", captured)
	}
}

func TestReadWakeMachineIDPlatformFailsEmptyOnErrno(t *testing.T) {
	stubDarwinGethostuuid(t, func(uuid *[16]byte, timeout *unix.Timespec) syscall.Errno {
		return unix.EWOULDBLOCK
	})
	if got := readWakeMachineIDPlatform(); got != "" {
		t.Fatalf("machine id = %q, want empty on errno", got)
	}
}

// Live check of the best-effort contract: a UUID-shaped identity or empty,
// never junk, and stable between reads.
func TestReadWakeMachineIDPlatformLiveValidOrEmpty(t *testing.T) {
	first := readWakeMachineIDPlatform()
	if first != "" && !isDarwinBootUUID(first) {
		t.Fatalf("machine id = %q, want UUID shape or empty", first)
	}
	if second := readWakeMachineIDPlatform(); second != first {
		t.Fatalf("machine id changed between reads: %q then %q", first, second)
	}
}
