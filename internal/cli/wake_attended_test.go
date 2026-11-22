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
// reminder scan writes nothing either. The raw path runs: no injector hook
// stands in, so the terminal-write record proves the transport is quiet.
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
	h.cfg.recordAttention = func(wakeAttentionEmission) error { return nil }

	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("attended seat typed %d doorbell(s), want none", got)
	}
	if len(h.injected) != 0 {
		t.Fatalf("attended seat routed mail to the raw terminal write: %q, want the notice only", h.injected)
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

// The external injector (--inject-via) is a transport too: on an attended
// seat its command never runs, and the mail routes to the notice.
func TestAttendedSeatNeverRunsTheExternalInjector(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	h.cfg.settings.attended = true
	attention := &strings.Builder{}
	h.cfg.attentionWrite = func(data []byte) (int, error) {
		attention.Write(data)
		return len(data), nil
	}
	h.cfg.attentionIsTTY = func() bool { return false }
	h.cfg.injectVia = "/bin/false"
	h.cfg.injectMode = wakeInjectModeRaw

	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("attended seat wrote to the terminal: %q, want nothing", h.injected)
	}
	if !strings.Contains(attention.String(), "a") {
		t.Fatalf("attention output = %q, want the mail notice", attention.String())
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

// The claude-native transport is gated too: on an attended seat a Notify
// that would send a frame never runs, and the mail routes to the one
// notice instead.
func TestAttendedSeatClaudeNativeSendsNoFrame(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	h.cfg.settings.attended = true
	notifies := 0
	h.cfg.native = countingNativeNotifier{onNotify: func() { notifies++ }}
	attention := &strings.Builder{}
	h.cfg.attentionWrite = func(data []byte) (int, error) {
		attention.Write(data)
		return len(data), nil
	}
	h.cfg.attentionIsTTY = func() bool { return false }

	h.send("a", format.PriorityNormal)
	h.cfg.injectMode = wakeInjectModeClaudeNative
	h.scan(h.cfg)
	if notifies != 0 {
		t.Fatalf("attended seat sent %d native frame(s), want none", notifies)
	}
	if !strings.Contains(attention.String(), "a") {
		t.Fatalf("attention output = %q, want the mail notice", attention.String())
	}
}

// countingNativeNotifier is a wakeNativeNotifier whose Notify the test
// counts; Observe and Close never fire in these scans.
type countingNativeNotifier struct {
	onNotify func()
}

func (n countingNativeNotifier) Notify(map[string]os.FileInfo, time.Time) (bool, error) {
	n.onNotify()
	return false, nil
}
func (countingNativeNotifier) Observe() (bool, error) { return false, nil }
func (countingNativeNotifier) Close() error           { return nil }

// The interrupt key is a transport: urgent mail on an attended seat with
// interrupt configured and no owner-bound doorbell types no 0x03, and the
// notice is the delivery.
func TestAttendedSeatNeverTypesTheInterruptKey(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	setAttendedForTest(t, h.root, "codex")
	h.cfg.settings.attended = true
	h.cfg.settings.interrupt = true
	h.cfg.settings.interruptPriority = "urgent"
	h.cfg.settings.interruptLabel = "interrupt"
	h.cfg.interruptKey = "\x03"
	h.cfg.settings.previewLen = 48
	attention := &strings.Builder{}
	h.cfg.attentionWrite = func(data []byte) (int, error) {
		attention.Write(data)
		return len(data), nil
	}
	h.cfg.attentionIsTTY = func() bool { return false }

	h.send("urgent", format.PriorityUrgent)
	h.scan(h.cfg)
	for _, typed := range h.injected {
		if strings.Contains(typed, "\x03") {
			t.Fatalf("attended seat typed the interrupt key: %q", typed)
		}
	}
	h.injected = nil
	if !strings.Contains(attention.String(), "urgent") {
		t.Fatalf("attention output = %q, want the urgent-mail notice", attention.String())
	}
}

// An Enter an unattended delivery owed, then attended turned on through the
// live reload, then the inbox drained and a scan: nothing is typed. The
// reload-time clear and the defence at the write site both gate it.
func TestAttendedSeatOnOwesNoEnterAfterTheDrain(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	h.cfg.settingsSource = func() ([]byte, bool, error) {
		raw, err := os.ReadFile(filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName))
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	}
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("unattended scan typed %d doorbell(s), want 1", got)
	}
	if err := os.WriteFile(
		filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName),
		[]byte(`{"schema":1,"settings":{"attended":true}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if !h.cfg.reloadSettings() || !h.cfg.settings.attended {
		t.Fatal("reloadSettings did not turn attended on")
	}
	h.drain()
	h.advance(time.Minute)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("attended seat after the drain typed %q, want nothing", h.injected)
	}
}

// The motivating case of a live attended change: a cohort parked at the
// typed ladder's lifetime cap (a person ignored eight doorbells), then
// attended turns on. The reload resets the cohort, and the settings tick's
// rescan delivers exactly one notice for what waits — not silence until a
// drain, and not a second notice at later scans.
func TestAttendedSeatOnNotifiesAParkedCohortAtTheLifetimeCap(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	h.cfg.settingsSource = func() ([]byte, bool, error) {
		raw, err := os.ReadFile(filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName))
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	}
	attention := &strings.Builder{}
	h.cfg.attentionWrite = func(data []byte) (int, error) {
		attention.Write(data)
		return len(data), nil
	}
	h.cfg.attentionIsTTY = func() bool { return false }
	h.cfg.recordAttention = func(wakeAttentionEmission) error { return nil }

	// Park the cohort, then spend the whole lifetime budget on it.
	h.send("old", format.PriorityNormal)
	h.scan(h.cfg)
	h.rings()
	for i := uint(0); h.cfg.doorbell.phase != wakeDoorbellParked && i < 20; i++ {
		h.advance(wakeDoorbellRetryMax)
		h.scan(h.cfg)
		h.rings()
	}
	if h.cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatalf("setup: phase = %v, want parked", h.cfg.doorbell.phase)
	}
	h.cfg.doorbell.attemptBudget = wakeDoorbellLifetimeAttemptCap

	// The person turns attended on through the live reload.
	if err := os.WriteFile(
		filepath.Join(fsq.AgentBase(h.root, "codex"), wakeSettingsFileName),
		[]byte(`{"schema":1,"settings":{"attended":true}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if !h.cfg.reloadSettings() || !h.cfg.settings.attended {
		t.Fatal("reloadSettings did not turn attended on")
	}
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("attended scan typed %d doorbell(s), want none", got)
	}
	emissions := func() int { return strings.Count(attention.String(), "AMQ [s]") }
	if got := emissions(); got != 1 {
		t.Fatalf("attended-on scan emitted %d notice(s) for the parked cohort, want 1", got)
	}
	h.advance(time.Minute)
	h.scan(h.cfg)
	if got := emissions(); got != 1 {
		t.Fatalf("a later scan emitted %d notice(s) total, want 1", got)
	}
}
