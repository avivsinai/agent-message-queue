package cli

// Purpose: the attended seat (bead agent-message-queue-gijx). On a seat with
// attended=true the wake never types: no doorbell text, no Enter, no
// interrupt key. Mail shows as a notice only, the Stop guard reports with a
// systemMessage and does not extend the turn, and the wait hook does not
// ring. The owner's own queued command stays typed (the one exception).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// setAttendedForTest writes attended=true into the agent's .wake.settings.
func setAttendedForTest(t *testing.T, root, me string) {
	t.Helper()
	path := filepath.Join(fsq.AgentBase(root, me), wakeSettingsFileName)
	raw, err := json.Marshal(map[string]any{"schema": 1, "settings": map[string]bool{"attended": true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Happy path: an attended seat is never typed into. Mail arrives, the wake
// emits the attention output, and the terminal writer records nothing; the
// reminder scan writes nothing either.
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

	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("attended seat typed %d doorbell(s), want none", got)
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

// The notice cadence (spec item O): one unread message over ten minutes of
// retries produces exactly one attention emission — not one per retry. The
// cohort parks after the first notice like a presented cohort; an addition
// (the reminder cohort) revives it for at most one more notice. The status
// is not degraded (item N): the notice is the designed channel.
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
	// An addition is mail the person has not been told about: one more notice,
	// then parked again.
	h.advance(time.Minute)
	h.send("b", format.PriorityNormal)
	h.scan(h.cfg)
	if got := emissions(); got > 2 {
		t.Fatalf("addition emitted %d notice(s) total, want at most 2", got)
	}
	for minute := 1; minute <= 10; minute++ {
		h.advance(time.Minute)
		h.scan(h.cfg)
	}
	if got := emissions(); got != 2 {
		t.Fatalf("ten more minutes emitted %d notice(s) total, want 2 (message + addition)", got)
	}
	if h.cfg.lastPersistedNotifierStatus == "degraded" {
		t.Fatal("attended notice recorded a degraded notifier status, want none")
	}
}
