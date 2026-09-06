package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestWakeDoorbellStateInjectedAcknowledgementSuppressesUnchangedCohort(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	first := wakeDoorbellTestFiles(t, "a.md", "b.md")
	var state wakeDoorbellState

	if plan := state.plan(now, first); !plan.attempt {
		t.Fatalf("initial plan = %#v", plan)
	}
	state.recordInjected(first)
	if state.phase != wakeDoorbellAnnounced {
		t.Fatalf("acknowledged phase = %v, want announced", state.phase)
	}
	if _, ok := state.nextDeadline(); ok {
		t.Fatal("acknowledged cohort retained a retry deadline")
	}
	if plan := state.plan(now.Add(10*wakeDoorbellRetryMax), first); plan.attempt {
		t.Fatalf("unchanged acknowledged cohort retried: %#v", plan)
	}

	remaining := map[string]os.FileInfo{"b.md": first["b.md"]}
	if plan := state.plan(now.Add(time.Second), remaining); plan.attempt {
		t.Fatalf("partial drain reannounced remaining cohort: %#v", plan)
	}
	if !sameKnownWakeCohort(state.cohort, remaining) {
		t.Fatalf("partial drain did not rebase announced cohort: %#v", state)
	}

	added := wakeDoorbellTestFiles(t, "c.md")
	expanded := map[string]os.FileInfo{
		"b.md": remaining["b.md"],
		"c.md": added["c.md"],
	}
	if plan := state.plan(now.Add(2*time.Second), expanded); !plan.attempt {
		t.Fatalf("new physical message did not rearm doorbell: %#v", plan)
	}
	state.recordInjected(expanded)
	if plan := state.plan(now.Add(3*time.Second), expanded); plan.attempt {
		t.Fatalf("expanded acknowledged cohort retried: %#v", plan)
	}
}
