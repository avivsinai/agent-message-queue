//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// The coop exec delivery line names an attended seat once, for every
// harness, with no prediction of what the hooks will deliver.
func TestPublishStartupDialogMarkerAttendedDeliveryLine(t *testing.T) {
	clearSendMailboxTestEnv(t)
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	for _, marker := range []wakeDialogMarker{
		{Schema: 1, Harness: "claude"},
		{Schema: 1, Harness: "codex"},
		{},
	} {
		_, stderr, err := captureEnvOutput(t, func() error {
			return publishStartupDialogMarker(h.root, "codex", marker, false, "")
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr, "dialog guard: plain\n") {
			t.Fatalf("stderr = %q, want the plain guard line", stderr)
		}
		if !strings.Contains(stderr, "delivery: attended (no doorbell is typed; mail shows as a notice)\n") {
			t.Fatalf("stderr = %q, want the universal attended delivery line", stderr)
		}
	}

	// A seat that is not attended prints nothing new.
	if err := os.Remove(filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName)); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := captureEnvOutput(t, func() error {
		return publishStartupDialogMarker(h.root, "codex", wakeDialogMarker{}, false, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "delivery: attended") {
		t.Fatalf("unattended stderr = %q, want no attended delivery line", stderr)
	}
}
