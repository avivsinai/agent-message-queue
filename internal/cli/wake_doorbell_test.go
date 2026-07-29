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

func TestWakeDoorbellStateRetriesUntilObserved(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md")
	tokens := []string{
		"11111111111111111111111111111111",
		"22222222222222222222222222222222",
	}
	nextToken := func() (string, error) {
		token := tokens[0]
		tokens = tokens[1:]
		return token, nil
	}
	var state wakeDoorbellState

	plan, err := state.plan(now, current, nextToken)
	if err != nil || !plan.attempt || plan.retry {
		t.Fatalf("initial plan = %#v, err=%v", plan, err)
	}
	state.recordAttempt(now)

	plan, err = state.plan(now.Add(wakeDoorbellRetryBase-time.Millisecond), current, nextToken)
	if err != nil || plan.attempt {
		t.Fatalf("early plan = %#v, err=%v", plan, err)
	}
	plan, err = state.plan(now.Add(wakeDoorbellRetryBase), current, nextToken)
	if err != nil || !plan.attempt || !plan.retry || plan.prompt != buildCoopWakeDoorbell(state.token) {
		t.Fatalf("retry plan = %#v, err=%v", plan, err)
	}

	if !state.observe(state.token, now.Add(wakeDoorbellRetryBase)) {
		t.Fatal("valid prompt observation was ignored")
	}
	observedUntil := state.observationUntil
	if state.observe(state.token, now.Add(time.Minute)) {
		t.Fatal("duplicate observation renewed the hold")
	}
	if state.observationUntil != observedUntil {
		t.Fatal("duplicate observation changed the deadline")
	}

	plan, err = state.plan(observedUntil.Add(-time.Millisecond), current, nextToken)
	if err != nil || plan.attempt {
		t.Fatalf("observed hold plan = %#v, err=%v", plan, err)
	}
	plan, err = state.plan(observedUntil, current, nextToken)
	if err != nil || !plan.attempt || plan.prompt != buildCoopWakeDoorbell("22222222222222222222222222222222") {
		t.Fatalf("expired observation plan = %#v, err=%v", plan, err)
	}
}

func TestWakeDoorbellStateRearmsOnAnyCohortProgress(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md", "b.md")
	tokenNumber := 0
	nextToken := func() (string, error) {
		tokenNumber++
		if tokenNumber == 1 {
			return "11111111111111111111111111111111", nil
		}
		return "22222222222222222222222222222222", nil
	}
	var state wakeDoorbellState
	if _, err := state.plan(now, current, nextToken); err != nil {
		t.Fatal(err)
	}
	state.recordAttempt(now)
	if !state.observe(state.token, now) {
		t.Fatal("observe initial generation")
	}

	remaining := map[string]os.FileInfo{
		"b.md": current["b.md"],
		"c.md": wakeDoorbellTestFiles(t, "c.md")["c.md"],
	}
	plan, err := state.plan(now.Add(time.Second), remaining, nextToken)
	if err != nil || !plan.attempt {
		t.Fatalf("progress plan = %#v, err=%v", plan, err)
	}
	if plan.prompt != buildCoopWakeDoorbell("22222222222222222222222222222222") {
		t.Fatalf("progress prompt = %q", plan.prompt)
	}
}

func TestWakeDoorbellStateRetriesTokenGenerationFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md")
	calls := 0
	nextToken := func() (string, error) {
		calls++
		if calls == 1 {
			return "", os.ErrInvalid
		}
		return "11111111111111111111111111111111", nil
	}
	var state wakeDoorbellState

	if plan, err := state.plan(now, current, nextToken); err == nil || plan.attempt {
		t.Fatalf("token failure plan = %#v, err=%v", plan, err)
	}
	if state.phase != wakeDoorbellAwaitingToken {
		t.Fatalf("token failure phase = %v, want awaiting token", state.phase)
	}
	if deadline, ok := state.nextDeadline(); !ok || deadline != now.Add(wakeDoorbellRetryBase) {
		t.Fatalf("token retry deadline = %s, %v", deadline, ok)
	}
	if plan, err := state.plan(now.Add(wakeDoorbellRetryBase-time.Millisecond), current, nextToken); err != nil || plan.attempt {
		t.Fatalf("early token retry plan = %#v, err=%v", plan, err)
	}
	plan, err := state.plan(now.Add(wakeDoorbellRetryBase), current, nextToken)
	if err != nil || !plan.attempt || plan.retry {
		t.Fatalf("token retry plan = %#v, err=%v", plan, err)
	}
}

func TestWakeDoorbellRetryDelayCaps(t *testing.T) {
	var previous time.Duration
	for attempt := uint(1); attempt <= 20; attempt++ {
		delay := wakeDoorbellRetryDelay(attempt)
		if delay < previous || delay > wakeDoorbellRetryMax {
			t.Fatalf("attempt %d delay = %s after %s", attempt, delay, previous)
		}
		previous = delay
	}
	if previous != wakeDoorbellRetryMax {
		t.Fatalf("final delay = %s, want %s", previous, wakeDoorbellRetryMax)
	}
}

func TestValidWakeDoorbellTokenRequiresCanonical128BitHex(t *testing.T) {
	for _, token := range []string{
		"",
		"0123456789abcdef",
		"0123456789abcdef0123456789abcdeg",
		"0123456789ABCDEF0123456789ABCDEF",
		"0123456789abcdef0123456789abcdef00",
	} {
		if validWakeDoorbellToken(token) {
			t.Fatalf("accepted noncanonical token %q", token)
		}
	}
	if !validWakeDoorbellToken("0123456789abcdef0123456789abcdef") {
		t.Fatal("rejected canonical token")
	}
}
