package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 7ja (field, 2026-10-05): every seat of a downstream CLI printed a manual
// naming reminder although naming was only the default. Default naming of
// an unknown CLI is quiet; an explicit --named keeps the reminder.
func TestCoopNamedUnknownCLIIsQuietByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AMQ_COOP_NAMED", "")
	_ = os.Unsetenv("AMQ_COOP_NAMED")
	for _, tc := range []struct {
		flagVisited  bool
		wantReminder bool
	}{{false, false}, {true, true}} {
		named, err := resolveCoopNamedEnabled(tc.flagVisited, true)
		if err != nil {
			t.Fatal(err)
		}
		_, stderr := captureOutput(t, func() error {
			_, err := applyCoopNamedBeforeExecAt(named, "downstream-cli", nil, "s1/lead", time.Now())
			return err
		})
		if got := strings.Contains(stderr, "manually"); got != tc.wantReminder {
			t.Fatalf("--named visited=%v: stderr %q, want reminder=%v", tc.flagVisited, stderr, tc.wantReminder)
		}
	}
}
