package cli

import (
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
	if state.phase != wakeDoorbellRecoveryRequired {
		t.Fatalf("recovery phase = %v", state.phase)
	}
	if state.planRecoveryAttention(now.Add(time.Millisecond), first) {
		t.Fatal("unchanged recovery cohort was repeated")
	}
	if state.planRecoveryAttention(now.Add(wakeDoorbellRetryBase-time.Millisecond), second) {
		t.Fatal("changed recovery cohort bypassed output rate bound")
	}
	if deadline, ok := state.nextDeadline(); !ok ||
		!deadline.Equal(now.Add(wakeDoorbellRetryBase)) {
		t.Fatalf("recovery deadline = %s, ok=%v", deadline, ok)
	}
	if state.planRecoveryAttention(now.Add(time.Millisecond), nil) {
		t.Fatal("empty recovery inbox emitted attention")
	}
	if _, ok := state.nextDeadline(); ok {
		t.Fatal("empty recovery inbox retained a pending alert deadline")
	}
	if state.planRecoveryAttention(now.Add(2*time.Millisecond), second) {
		t.Fatal("drain-and-arrive recovery flap bypassed output rate bound")
	}
	if !state.planRecoveryAttention(now.Add(wakeDoorbellRetryBase), second) {
		t.Fatal("changed recovery cohort was not emitted after rate bound")
	}
	state.recordRecoveryRequired(now.Add(wakeDoorbellRetryBase), second)
	if state.planRecoveryAttention(now.Add(2*wakeDoorbellRetryBase), second) {
		t.Fatal("recorded recovery cohort repeated")
	}
}

func TestWakeDoorbellRetryCadenceCapsPerDeliveredChannel(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name   string
		record func(*wakeDoorbellState, time.Time)
		wants  []time.Duration
	}{
		{
			name: "input",
			record: func(state *wakeDoorbellState, attemptAt time.Time) {
				state.recordAttempt(attemptAt)
			},
			wants: []time.Duration{
				5 * time.Second,
				10 * time.Second,
				20 * time.Second,
				40 * time.Second,
				80 * time.Second,
				2 * time.Minute,
				2 * time.Minute,
			},
		},
		{
			name: "attention",
			record: func(state *wakeDoorbellState, attemptAt time.Time) {
				state.recordAttentionAttempt(attemptAt)
			},
			wants: []time.Duration{
				30 * time.Second,
				time.Minute,
				2 * time.Minute,
				4 * time.Minute,
				8 * time.Minute,
				15 * time.Minute,
				15 * time.Minute,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state wakeDoorbellState
			for attempt, want := range tt.wants {
				tt.record(&state, now)
				if got := state.nextAttempt.Sub(now); got != want {
					t.Fatalf(
						"attempt %d delay = %s, want %s",
						attempt+1,
						got,
						want,
					)
				}
			}
		})
	}
}
