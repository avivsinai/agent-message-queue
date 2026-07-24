//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package registry

import (
	"errors"
	"testing"
)

func TestFlockFailsClosedWhenUnsupported(t *testing.T) {
	if err := flockExclusive(nil); !errors.Is(err, errCrossProcessLockUnsupported) {
		t.Fatalf("flockExclusive error = %v, want unsupported error", err)
	}
	acquired, err := flockTryExclusive(nil)
	if acquired || !errors.Is(err, errCrossProcessLockUnsupported) {
		t.Fatalf("flockTryExclusive = (%v, %v), want (false, unsupported error)", acquired, err)
	}
	flockRelease(nil)
}
