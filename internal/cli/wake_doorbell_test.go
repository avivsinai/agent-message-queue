package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type wakeIdentityUnavailableFileInfo struct {
	name string
}

func (info wakeIdentityUnavailableFileInfo) Name() string  { return info.name }
func (wakeIdentityUnavailableFileInfo) Size() int64        { return 0 }
func (wakeIdentityUnavailableFileInfo) Mode() os.FileMode  { return 0o600 }
func (wakeIdentityUnavailableFileInfo) ModTime() time.Time { return time.Time{} }
func (wakeIdentityUnavailableFileInfo) IsDir() bool        { return false }
func (wakeIdentityUnavailableFileInfo) Sys() any           { return nil }

func wakeDoorbellTestFiles(t *testing.T, names ...string) map[string]os.FileInfo {
	t.Helper()
	dir := t.TempDir()
	current := make(map[string]os.FileInfo, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		current[name] = info
	}
	return current
}

func TestWakeDoorbellStateRetriesUntilInboxProgress(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md", "b.md")
	var state wakeDoorbellState

	plan := state.plan(now, current)
	if !plan.attempt || plan.prompt != coopWakeDoorbell {
		t.Fatalf("initial plan = %#v", plan)
	}
	state.recordAttempt(now)

	plan = state.plan(now.Add(wakeDoorbellRetryBase-time.Millisecond), current)
	if plan.attempt {
		t.Fatalf("early retry plan = %#v", plan)
	}
	plan = state.plan(now.Add(wakeDoorbellRetryBase), current)
	if !plan.attempt || plan.prompt != coopWakeDoorbell {
		t.Fatalf("due retry plan = %#v", plan)
	}

	remaining := map[string]os.FileInfo{"b.md": current["b.md"]}
	plan = state.plan(now.Add(wakeDoorbellRetryBase+time.Millisecond), remaining)
	if !plan.attempt {
		t.Fatalf("progress plan = %#v", plan)
	}
	if state.attempts != 0 || len(state.cohort) != 1 {
		t.Fatalf("progress state = %#v", state)
	}

	if plan = state.plan(now.Add(time.Minute), nil); plan.attempt {
		t.Fatalf("empty inbox plan = %#v", plan)
	}
	if state.phase != wakeDoorbellIdle {
		t.Fatalf("empty inbox state = %#v", state)
	}
}

func TestWakeDoorbellStateCoalescesAddedMessageUntilExistingDeadline(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	first := wakeDoorbellTestFiles(t, "a.md")
	var state wakeDoorbellState

	if plan := state.plan(now, first); !plan.attempt {
		t.Fatalf("initial plan = %#v", plan)
	}
	state.recordAttempt(now)
	lastAttempt := now
	for state.attempts < 3 {
		lastAttempt = state.nextAttempt
		if plan := state.plan(lastAttempt, first); !plan.attempt {
			t.Fatalf("ladder attempt %d plan = %#v", state.attempts+1, plan)
		}
		state.recordAttempt(lastAttempt)
	}
	decayedDeadline := state.nextAttempt

	added := wakeDoorbellTestFiles(t, "b.md", "c.md", "d.md")
	current := map[string]os.FileInfo{
		"a.md": first["a.md"],
		"b.md": added["b.md"],
		"c.md": added["c.md"],
	}
	additionScannedAt := lastAttempt.Add(time.Second)
	plan := state.plan(additionScannedAt, current)
	if plan.attempt || plan.progress {
		t.Fatalf("addition emitted before delivery floor: %#v", plan)
	}
	if state.attempts != 3 || !sameKnownWakeCohort(state.cohort, current) {
		t.Fatalf("addition did not extend the pending cohort: %#v", state)
	}
	floorDeadline := lastAttempt.Add(wakeDoorbellRetryBase)
	if !state.nextAttempt.Equal(floorDeadline) {
		t.Fatalf(
			"addition deadline = %s, want floor %s instead of decayed %s",
			state.nextAttempt,
			floorDeadline,
			decayedDeadline,
		)
	}

	burst := map[string]os.FileInfo{
		"a.md": first["a.md"],
		"b.md": added["b.md"],
		"c.md": added["c.md"],
		"d.md": added["d.md"],
	}
	plan = state.plan(additionScannedAt.Add(time.Millisecond), burst)
	if plan.attempt || plan.progress {
		t.Fatalf("same-burst addition emitted another doorbell: %#v", plan)
	}
	if state.attempts != 3 || !state.nextAttempt.Equal(floorDeadline) {
		t.Fatalf("same-burst addition changed retry ladder: %#v", state)
	}

	plan = state.plan(floorDeadline, burst)
	if !plan.attempt || plan.progress || plan.prompt != coopWakeDoorbell {
		t.Fatalf("consolidated floor plan = %#v", plan)
	}
	state.recordAttempt(floorDeadline)
	if plan = state.plan(floorDeadline.Add(time.Millisecond), burst); plan.attempt {
		t.Fatalf("coalesced burst emitted more than one doorbell: %#v", plan)
	}

	remaining := map[string]os.FileInfo{"d.md": burst["d.md"]}
	plan = state.plan(floorDeadline.Add(2*time.Millisecond), remaining)
	if !plan.attempt || !plan.progress || plan.prompt != coopWakeDoorbell {
		t.Fatalf("post-expansion drain plan = %#v", plan)
	}
	if state.attempts != 0 || !sameKnownWakeCohort(state.cohort, remaining) {
		t.Fatalf("post-expansion drain did not rearm remaining cohort: %#v", state)
	}
}

func TestWakeDoorbellStateParksUnchangedCohortAfterFourAttempts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md")
	var state wakeDoorbellState

	wantAttempts := []time.Time{
		now,
		now.Add(5 * time.Second),
		now.Add(15 * time.Second),
		now.Add(35 * time.Second),
	}
	for attempt, attemptAt := range wantAttempts {
		plan := state.plan(attemptAt, current)
		if !plan.attempt {
			t.Fatalf("attempt %d plan = %#v", attempt+1, plan)
		}
		state.recordAttempt(attemptAt)
	}

	if plan := state.plan(now.Add(24*time.Hour), current); plan.attempt {
		t.Fatalf("unchanged cohort exceeded four-attempt budget: %#v", plan)
	}
}

func TestWakeDoorbellStateParkedAdditionTopsUpOneAttemptToLifetimeCap(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md")
	var state wakeDoorbellState

	for attempt := 0; attempt < 4; attempt++ {
		attemptAt := now
		if attempt > 0 {
			attemptAt = state.nextAttempt
		}
		if plan := state.plan(attemptAt, current); !plan.attempt {
			t.Fatalf("initial attempt %d plan = %#v", attempt+1, plan)
		}
		state.recordAttempt(attemptAt)
		now = attemptAt
	}
	if plan := state.plan(now.Add(time.Hour), current); plan.attempt {
		t.Fatalf("initial cohort did not park: %#v", plan)
	}

	for addition := 1; addition <= 4; addition++ {
		name := fmt.Sprintf("added-%d.md", addition)
		current[name] = wakeDoorbellTestFiles(t, name)[name]
		scannedAt := now.Add(time.Second)
		if plan := state.plan(scannedAt, current); plan.attempt {
			t.Fatalf("addition %d bypassed delivery floor: %#v", addition, plan)
		}
		attemptAt := state.nextAttempt
		if plan := state.plan(attemptAt, current); !plan.attempt {
			t.Fatalf("addition %d did not receive one top-up attempt: %#v", addition, plan)
		}
		state.recordAttempt(attemptAt)
		now = attemptAt
		if plan := state.plan(now.Add(time.Hour), current); plan.attempt {
			t.Fatalf("addition %d received more than one top-up: %#v", addition, plan)
		}
	}

	name := "beyond-lifetime-cap.md"
	current[name] = wakeDoorbellTestFiles(t, name)[name]
	if plan := state.plan(now.Add(time.Hour), current); plan.attempt {
		t.Fatalf("addition exceeded eight-attempt lifetime cap: %#v", plan)
	}
}

func TestWakeDoorbellStateDrainResetsReminderBudget(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md")
	var state wakeDoorbellState

	for attempt := 0; attempt < 4; attempt++ {
		attemptAt := now
		if attempt > 0 {
			attemptAt = state.nextAttempt
		}
		if plan := state.plan(attemptAt, current); !plan.attempt {
			t.Fatalf("first cohort attempt %d plan = %#v", attempt+1, plan)
		}
		state.recordAttempt(attemptAt)
		now = attemptAt
	}
	if plan := state.plan(now.Add(time.Hour), nil); plan.attempt {
		t.Fatalf("drain plan = %#v", plan)
	}

	fresh := wakeDoorbellTestFiles(t, "fresh.md")
	if plan := state.plan(now.Add(time.Hour+time.Second), fresh); !plan.attempt {
		t.Fatalf("fresh cohort inherited exhausted budget: %#v", plan)
	}
	if state.attempts != 0 {
		t.Fatalf("fresh cohort attempts = %d, want 0 before first record", state.attempts)
	}
}

func TestWakeDoorbellParkedStatusPublishesAndClears(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md")
	var statuses []wakeDoorbellStatus
	cfg := wakeConfig{
		recordDoorbellStatus: func(parked bool, attempts uint) error {
			statuses = append(statuses, wakeDoorbellStatus{parked: parked, attempts: attempts})
			return nil
		},
	}
	cfg.doorbell.arm(current)
	for attempt := 0; attempt < 4; attempt++ {
		recordWakeAttempt(&cfg, now, current, nil)
		now = cfg.doorbell.nextAttempt
	}
	if len(statuses) != 1 || statuses[0] != (wakeDoorbellStatus{parked: true, attempts: 4}) {
		t.Fatalf("parked statuses = %#v", statuses)
	}

	cfg.doorbell.reset()
	if err := persistWakeDoorbellStatus(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[1] != (wakeDoorbellStatus{}) {
		t.Fatalf("cleared statuses = %#v", statuses)
	}
}

func TestWakeDoorbellStateRearmsWhenUnknownIdentityBecomesKnown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := map[string]os.FileInfo{
		"readable.md": wakeIdentityUnavailableFileInfo{name: "readable.md"},
	}
	var state wakeDoorbellState
	if plan := state.plan(now, current); !plan.attempt {
		t.Fatalf("initial plan = %#v", plan)
	}
	state.recordAttempt(now)
	if _, ok := state.cohort["readable.md"]; !ok {
		t.Fatal("identity-unavailable member was dropped from the cohort")
	}

	known := wakeDoorbellTestFiles(t, "readable.md")
	plan := state.plan(now.Add(time.Second), known)
	if !plan.attempt {
		t.Fatalf("identity recovery plan = %#v", plan)
	}
}

func TestWakeDoorbellStateKeepsRetryWhenKnownIdentityBecomesUnavailable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "readable.md")
	var state wakeDoorbellState
	if plan := state.plan(now, current); !plan.attempt {
		t.Fatalf("initial plan = %#v", plan)
	}
	state.recordAttempt(now)

	temporarilyUnknown := map[string]os.FileInfo{"readable.md": nil}
	plan := state.plan(now.Add(wakeDoorbellRetryBase), temporarilyUnknown)
	if !plan.attempt {
		t.Fatalf("temporarily unknown plan = %#v", plan)
	}
}

func TestWakeDoorbellRecoveryAttentionIsCohortBoundedAndRateLimited(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	first := wakeDoorbellTestFiles(t, "first.md")
	second := map[string]os.FileInfo{
		"first.md":  first["first.md"],
		"second.md": wakeDoorbellTestFiles(t, "second.md")["second.md"],
	}
	var state wakeDoorbellState

	if !state.planRecoveryAttention(now, first) {
		t.Fatal("initial recovery cohort was suppressed")
	}
	state.recordRecoveryRequired(now, first)
	state.recordRecoveryAttentionDelivered()
	if state.phase != wakeDoorbellRecoveryRequired {
		t.Fatalf("recovery phase = %v", state.phase)
	}
	if state.planRecoveryAttention(now.Add(time.Millisecond), first) {
		t.Fatal("unchanged recovery cohort was repeated")
	}
	if state.planRecoveryAttention(now.Add(wakeDoorbellAttentionRetryBase-time.Millisecond), second) {
		t.Fatal("changed recovery cohort bypassed output rate bound")
	}
	if deadline, ok := state.nextDeadline(); !ok ||
		!deadline.Equal(now.Add(wakeDoorbellAttentionRetryBase)) {
		t.Fatalf("recovery deadline = %s, ok=%v", deadline, ok)
	}
	if state.planRecoveryAttention(now.Add(time.Millisecond), nil) {
		t.Fatal("empty recovery inbox emitted attention")
	}
	if _, ok := state.nextDeadline(); ok {
		t.Fatal("empty recovery inbox retained a pending alert deadline")
	}
	if state.phase != wakeDoorbellIdle {
		t.Fatalf("empty recovery inbox phase = %v, want idle", state.phase)
	}
	if !state.planRecoveryAttention(now.Add(2*time.Millisecond), second) {
		t.Fatal("new recovery cohort inherited the drained cohort's rate bound")
	}
	recordedAt := now.Add(2 * time.Millisecond)
	state.recordRecoveryRequired(recordedAt, second)
	if state.planRecoveryAttention(
		recordedAt.Add(wakeDoorbellAttentionRetryBase-time.Millisecond),
		second,
	) {
		t.Fatal("recorded recovery cohort repeated")
	}
}

func TestWakeDoorbellAttentionRetryCadenceCaps(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	wants := []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		15 * time.Minute,
		15 * time.Minute,
	}
	var state wakeDoorbellState
	for attempt, want := range wants {
		state.recordAttentionAttempt(now)
		if got := state.nextAttempt.Sub(now); got != want {
			t.Fatalf(
				"attempt %d delay = %s, want %s",
				attempt+1,
				got,
				want,
			)
		}
	}
}
