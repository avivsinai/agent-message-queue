//go:build darwin || linux

package cli

// Purpose: the attended seat. On a seat with
// attended=true the wake never types: no doorbell text, no Enter, no
// interrupt key. Mail shows as a notice only, the Stop guard reports with a
// systemMessage and does not extend the turn, and the wait hook does not
// ring. The owner's own queued command stays typed (the one exception).

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Happy path: an attended seat is never typed into. Mail arrives, the wake
// emits the attention output, and the terminal writer records nothing; the
// reminder scan writes nothing either. The external injector (--inject-via)
// is a transport too, so an attended seat routes it to the notice as well.
func TestAttendedSeatNeverTypesTheDoorbell(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	h.cfg.settings.attended = true
	attention := &strings.Builder{}
	h.cfg.attentionWrite = func(data []byte) (int, error) {
		attention.Write(data)
		return len(data), nil
	}
	h.cfg.attentionIsTTY = func() bool { return false }
	h.cfg.injectViaHook = func(text string) (string, error) {
		h.injected = append(h.injected, text)
		return "", nil
	}

	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("attended seat typed %d doorbell(s), want none", got)
	}
	if len(h.injected) != 0 {
		t.Fatalf("attended seat routed mail to the injector: %q, want the notice only", h.injected)
	}
	if !strings.Contains(attention.String(), "a") {
		t.Fatalf("attention output = %q, want the mail notice", attention.String())
	}
	if !h.cfg.lastAttemptAttention {
		t.Fatal("attended delivery did not record attention")
	}
	h.advance(time.Minute)
	h.send("b", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("attended reminder typed %d doorbell(s), want none", got)
	}
}

// The owner's own queued command is the one exception: it is the owner's
// input, typed on the owner's order, on an attended seat too. The command
// path builds its own notification (wakePayloadOwnerCommand), so it bypasses
// the attended gate in deliverWakeNotification.
func TestAttendedSeatStillTypesTheOwnerCommand(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	h.cfg.settings.attended = true
	owner, target := commandSeat(t, h)
	h.cfg.dialogSource = func(time.Time) (wakeDialogState, error) { return wakeDialogClosed, nil }
	queueCommand(t, h, owner, target, "/compact", h.now)

	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	served := h.injected
	h.injected = nil
	serveWakeCommands(h.cfg)
	if want := []string{"/compact", "\r"}; !slices.Equal(h.injected, want) {
		t.Fatalf("attended seat typed %q for mail %q, want the owner command %q and no doorbell", h.injected, served, want)
	}
}

// The notice cadence: one unread message over ten minutes of retries
// produces exactly one attention emission — not one per retry. The cohort
// is recorded as announced after the first notice; an addition is mail the
// person has not been told about and gives exactly one more notice. There
// is no lifetime cap on the attended notice: the cap bounds synthetic
// input, and an attended seat types none.
func TestAttendedSeatNoticeCadenceOverTenMinutes(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	h.cfg.settings.attended = true
	attention := &strings.Builder{}
	h.cfg.attentionWrite = func(data []byte) (int, error) {
		attention.Write(data)
		return len(data), nil
	}
	h.cfg.attentionIsTTY = func() bool { return false }
	var statuses []string
	h.cfg.recordNotifierStatus = func(status, mode, reason string) error {
		statuses = append(statuses, status)
		return nil
	}

	emissions := func() int { return strings.Count(attention.String(), "AMQ [s]") }
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if got := emissions(); got != 1 {
		t.Fatalf("first scan emitted %d notice(s), want 1", got)
	}
	for minute := 1; minute <= 10; minute++ {
		h.advance(time.Minute)
		h.scan(h.cfg)
	}
	if got := emissions(); got != 1 {
		t.Fatalf("ten minutes of retries emitted %d notice(s), want 1 for the unchanged message", got)
	}
	// Ten more messages, none drained: each gets its one notice.
	for i := 0; i < 10; i++ {
		h.advance(10 * time.Minute)
		h.send(fmt.Sprintf("m%d", i), format.PriorityNormal)
		h.scan(h.cfg)
	}
	if got := emissions(); got != 11 {
		t.Fatalf("ten additions emitted %d notice(s) total, want 11 (one per cohort)", got)
	}
	for _, status := range statuses {
		if status == "degraded" {
			t.Fatal("attended notice recorded a degraded notifier status, want none")
		}
	}
}

// A live attended→off change lets the doorbell ring again: the next scan
// announces the pending cohort fresh and types one doorbell.
func TestAttendedSeatOffRingsOneDoorbellForThePendingCohort(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	h.cfg.settingsSource = func() ([]byte, bool, error) {
		raw, err := os.ReadFile(filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName))
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	}
	if changed := h.cfg.reloadSettings(); !changed || !h.cfg.settings.attended {
		t.Fatalf("attended = %v, changed = %v; want the file's attended", h.cfg.settings.attended, changed)
	}
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("attended scan typed %d doorbell(s), want none", got)
	}
	// Turn attended off in the same file the live reload reads.
	if err := os.WriteFile(
		filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName),
		[]byte(`{"schema":1,"settings":{"attended":false}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if h.cfg.reloadSettings() != true {
		t.Fatal("reloadSettings did not report the change")
	}
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("attended-off scan typed %d doorbell(s), want 1 for the pending cohort", got)
	}
}
