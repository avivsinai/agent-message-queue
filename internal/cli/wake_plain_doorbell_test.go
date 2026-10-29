//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// Owner ruling on bead agent-message-queue-r6h: seats looked unwakeable
// because the wake held its doorbell without any hook installed, for a
// Codex startup screen only a hook could release and for a marker older
// than the quiet hold. With no AMQ dialog hook the wake rings a plain
// doorbell: text and Enter, never held for a dialog.
func TestWakeRingsAPlainDoorbellWithoutDialogHooks(t *testing.T) {
	h, _, real, marker := newCodexTrustDialogHarness(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	// An untrusted folder outside any git checkout, where Codex shows its
	// trust screen.
	if err := os.Remove(filepath.Join(real, ".git")); err != nil {
		t.Fatal(err)
	}
	// A Claude Code Startup marker an earlier launch left behind.
	stale := `{"schema":1,"event":"Startup","harness":"claude","cwd":"` + real + `","opened_at":"x"}` + "\n"
	if err := os.WriteFile(marker, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := h.now.Add(-wakeDialogQuietHold - time.Minute)
	if err := os.Chtimes(marker, opened, opened); err != nil {
		t.Fatal(err)
	}
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	// Codex's ring: the text, its LF submit prelude, and the Enters.
	if want := []string{coopWakeDoorbell, "\n", "\r", "\r"}; !slices.Equal(h.injected, want) {
		t.Fatalf("injected %q, want the plain doorbell %q", h.injected, want)
	}

	_, stderr, err := captureEnvOutput(t, func() error {
		return syncStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", []string{"-c", "check_for_update_on_startup=false"}, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(marker); !os.IsNotExist(err) {
		t.Fatalf("marker after a launch without dialog hooks = %q, %v; want none", raw, err)
	}
	if !strings.Contains(stderr, "dialog guard: plain\n") {
		t.Fatalf("stderr = %q, want the plain dialog guard line", stderr)
	}
}

// Pre-merge review of r6h PR 1: an agy marker left by a relay killed mid
// turn held a plain seat with no end. It read as unknown after 10 minutes
// and stayed held, and a plain agy launch, which runs no relay, never
// removed it. An aged agy marker now reads as closed, and a plain agy launch
// removes a leftover one.
func TestWakeRingsPastAnAgyMarkerAKilledRelayLeft(t *testing.T) {
	h, _, _, marker := newCodexTrustDialogHarness(t)
	left := `{"schema":1,"event":"OSC7501","harness":"agy","kind":"working","opened_at":"2026-10-26T00:00:00Z"}` + "\n"
	write := func(opened time.Time) {
		t.Helper()
		if err := os.WriteFile(marker, []byte(left), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(marker, opened, opened); err != nil {
			t.Fatal(err)
		}
	}
	write(h.now.Add(-wakeDialogQuietHold - time.Minute))
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("doorbells past an aged agy marker = %d, want 1", got)
	}
	h.drain()

	write(h.now)
	if err := syncStartupDialogMarker(h.root, "codex", "/usr/local/bin/agy", nil, false); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(marker); !os.IsNotExist(err) {
		t.Fatalf("agy marker after a plain agy launch = %q, %v; want none", raw, err)
	}
	h.send("b", format.PriorityNormal)
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("doorbells after a plain agy launch = %d, want 1", got)
	}
}
