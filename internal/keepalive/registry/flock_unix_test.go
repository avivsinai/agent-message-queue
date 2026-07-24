//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFlockTryExclusiveReportsBusyAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open first lock: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open second lock: %v", err)
	}
	defer func() { _ = second.Close() }()

	if err := flockExclusive(first); err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	acquired, err := flockTryExclusive(second)
	if err != nil {
		flockRelease(first)
		t.Fatalf("try second lock: %v", err)
	}
	if acquired {
		flockRelease(second)
		flockRelease(first)
		t.Fatal("second lock acquired while first lock was held")
	}

	flockRelease(first)
	acquired, err = flockTryExclusive(second)
	if err != nil {
		t.Fatalf("try second lock after release: %v", err)
	}
	if !acquired {
		t.Fatal("second lock remained busy after first lock was released")
	}
	flockRelease(second)
}
