//go:build !darwin && !linux

package app

import (
	"fmt"
	"runtime"
	"testing"
)

func TestSelfUpgradeIsIneligibleOnUnsupportedPlatform(t *testing.T) {
	controller := newSelfUpgradeController("", "", "1.0.0", true)
	if controller.eligible {
		t.Fatal("unsupported platform controller is eligible")
	}
	want := fmt.Sprintf("self-upgrade is unsupported on %s", runtime.GOOS)
	if controller.reason != want {
		t.Fatalf("controller reason = %q, want %q", controller.reason, want)
	}
}
