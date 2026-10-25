//go:build darwin || linux

package cli

import (
	"fmt"
	"testing"
	"time"
)

// Startup-guard review found that the newer-cache test stopped at a missing
// binary. These observations reach the actual cache decision after version
// verification, including its positive release paths.
func TestCodexVersionCacheDecidesTheUpdatePrompt(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, latest, dismissed string
		installed               [3]uint64
		ruledOut                bool
	}{
		{"newer", "0.164.0", "", [3]uint64{0, 163, 0}, false},
		{"current", "0.163.0", "", [3]uint64{0, 163, 0}, true},
		{"dismissed", "0.164.0", "0.164.0", [3]uint64{0, 163, 0}, true},
		{"unverified installed version", "0.162.0", "", [3]uint64{0, 162, 0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"latest_version":%q,"dismissed_version":%q,"last_checked_at":%q}`, tc.latest, tc.dismissed, now.Format(time.RFC3339)))
			if got := codexCachedVersionRulesOutPrompt(raw, tc.installed, now); got != tc.ruledOut {
				t.Fatalf("prompt ruled out = %v, want %v", got, tc.ruledOut)
			}
		})
	}
}
