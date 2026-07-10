//go:build darwin

package cli

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func stubDarwinBootIdentityReaders(
	t *testing.T,
	session func() (string, error),
	bootTime func() (*unix.Timeval, error),
) {
	t.Helper()
	oldSession := readDarwinBootSessionUUID
	oldBootTime := readDarwinBootTime
	readDarwinBootSessionUUID = session
	readDarwinBootTime = bootTime
	t.Cleanup(func() {
		readDarwinBootSessionUUID = oldSession
		readDarwinBootTime = oldBootTime
	})
}

func TestDarwinBootIdentityPrefersBootSessionUUIDAndKeepsLegacyAlias(t *testing.T) {
	bootTime := unix.NsecToTimeval(1783327533407566000)
	stubDarwinBootIdentityReaders(t,
		func() (string, error) { return "  9C0682F4-901B-4243-8B5C-287FAFB9AD0E\n", nil },
		func() (*unix.Timeval, error) { return &bootTime, nil },
	)

	bootID, legacyBootID := darwinBootIdentity()
	if bootID != "9C0682F4-901B-4243-8B5C-287FAFB9AD0E" {
		t.Fatalf("boot id = %q", bootID)
	}
	if legacyBootID != "1783327533.407566000" {
		t.Fatalf("legacy boot id = %q", legacyBootID)
	}
}

func TestDarwinBootIdentityFallsBackToBootTime(t *testing.T) {
	bootTime := unix.NsecToTimeval(1783327533407566000)
	stubDarwinBootIdentityReaders(t,
		func() (string, error) { return "", errors.New("unsupported") },
		func() (*unix.Timeval, error) { return &bootTime, nil },
	)

	bootID, legacyBootID := darwinBootIdentity()
	if bootID != "1783327533.407566000" {
		t.Fatalf("fallback boot id = %q", bootID)
	}
	if legacyBootID != "" {
		t.Fatalf("fallback legacy boot id = %q, want empty", legacyBootID)
	}
}

func TestDarwinBootIdentityReturnsEmptyWhenBothSourcesUnavailable(t *testing.T) {
	stubDarwinBootIdentityReaders(t,
		func() (string, error) { return "\x00", nil },
		func() (*unix.Timeval, error) { return nil, errors.New("unsupported") },
	)

	bootID, legacyBootID := darwinBootIdentity()
	if bootID != "" || legacyBootID != "" {
		t.Fatalf("boot ids = %q, %q; want empty", bootID, legacyBootID)
	}
}
