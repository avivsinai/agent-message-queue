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

func TestWakeDoorbellStateRetriesUntilObserved(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md")
	tokenCalls := 0
	tokens := []string{
		"11111111111111111111111111111111",
		"22222222222222222222222222222222",
	}
	nextToken := func() (string, error) {
		tokenCalls++
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
	if err != nil ||
		!plan.attempt ||
		!plan.retry ||
		plan.prompt != buildCoopWakeDoorbell("11111111111111111111111111111111") {
		t.Fatalf("expired observation plan = %#v, err=%v", plan, err)
	}
	if tokenCalls != 1 {
		t.Fatalf("token generation calls = %d, want 1 without cohort progress", tokenCalls)
	}
	if state.attempts != 1 || len(state.cohort) != 1 {
		t.Fatalf("expired observation state = %#v, want preserved attempt and cohort", state)
	}
	if state.observe(state.token, observedUntil.Add(time.Second)) {
		t.Fatal("expired generation accepted a second observation lease")
	}
	if !state.observationUntil.IsZero() {
		t.Fatalf("expired generation renewed observation until %s", state.observationUntil)
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

func TestWakeDoorbellStateKeepsPendingMembershipWithoutFileIdentity(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := map[string]os.FileInfo{"readable.md": nil}
	tokenCalls := 0
	var state wakeDoorbellState

	plan, err := state.plan(now, current, func() (string, error) {
		tokenCalls++
		return "11111111111111111111111111111111", nil
	})
	if err != nil || !plan.attempt || plan.retry {
		t.Fatalf("identity-unavailable plan = %#v, err=%v", plan, err)
	}
	if tokenCalls != 1 || len(state.cohort) != 1 {
		t.Fatalf("identity-unavailable state = %#v, token calls=%d", state, tokenCalls)
	}
	if _, ok := state.cohort["readable.md"]; !ok {
		t.Fatal("readable pending filename was dropped from the doorbell cohort")
	}
}

func TestWakeDoorbellStateRearmsWhenUnknownIdentityBecomesKnown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := map[string]os.FileInfo{
		"readable.md": wakeIdentityUnavailableFileInfo{name: "readable.md"},
	}
	tokens := []string{
		"11111111111111111111111111111111",
		"22222222222222222222222222222222",
	}
	tokenCalls := 0
	nextToken := func() (string, error) {
		token := tokens[tokenCalls]
		tokenCalls++
		return token, nil
	}
	var state wakeDoorbellState
	if _, err := state.plan(now, current, nextToken); err != nil {
		t.Fatal(err)
	}

	known := wakeDoorbellTestFiles(t, "readable.md")
	plan, err := state.plan(now.Add(time.Second), known, nextToken)
	if err != nil || !plan.attempt || plan.retry {
		t.Fatalf("identity recovery plan = %#v, err=%v", plan, err)
	}
	if tokenCalls != 2 ||
		plan.prompt != buildCoopWakeDoorbell("22222222222222222222222222222222") {
		t.Fatalf("identity recovery plan = %#v, token calls=%d; want rearmed generation", plan, tokenCalls)
	}
}

func TestWakeDoorbellStateKeepsGenerationWhenKnownIdentityBecomesUnavailable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "readable.md")
	tokenCalls := 0
	nextToken := func() (string, error) {
		tokenCalls++
		return "11111111111111111111111111111111", nil
	}
	var state wakeDoorbellState
	if _, err := state.plan(now, current, nextToken); err != nil {
		t.Fatal(err)
	}
	state.recordAttempt(now)

	temporarilyUnknown := map[string]os.FileInfo{"readable.md": nil}
	plan, err := state.plan(now.Add(wakeDoorbellRetryBase), temporarilyUnknown, nextToken)
	if err != nil || !plan.attempt || !plan.retry {
		t.Fatalf("temporarily unknown plan = %#v, err=%v", plan, err)
	}
	if tokenCalls != 1 ||
		plan.prompt != buildCoopWakeDoorbell("11111111111111111111111111111111") {
		t.Fatalf("temporarily unknown plan = %#v, token calls=%d; want preserved generation", plan, tokenCalls)
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

func TestWakeDoorbellStateGatesDeliveredCohortAndRearmsOnChange(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	current := wakeDoorbellTestFiles(t, "a.md")
	var state wakeDoorbellState

	if !state.planCohortDelivery(current) {
		t.Fatal("initial cohort delivery was suppressed")
	}
	state.recordCohortDelivered(current)
	if state.planCohortDelivery(current) {
		t.Fatal("unchanged delivered cohort was redelivered")
	}
	if _, ok := state.nextDeadline(); ok {
		t.Fatal("delivered cohort retained a retry deadline")
	}
	if state.observe("11111111111111111111111111111111", now) {
		t.Fatal("delivered cohort accepted an input observation")
	}

	added := map[string]os.FileInfo{
		"a.md": current["a.md"],
		"b.md": wakeDoorbellTestFiles(t, "b.md")["b.md"],
	}
	if !state.planCohortDelivery(added) {
		t.Fatal("cohort addition was suppressed")
	}
	state.recordCohortDelivered(added)
	replacement := map[string]os.FileInfo{
		"a.md": added["a.md"],
		"b.md": wakeDoorbellTestFiles(t, "b.md")["b.md"],
	}
	if !state.planCohortDelivery(replacement) {
		t.Fatal("cohort replacement was suppressed")
	}

	plan, err := state.plan(now, replacement, func() (string, error) {
		return "22222222222222222222222222222222", nil
	})
	if err != nil || !plan.attempt || plan.retry {
		t.Fatalf("re-entered token plan = %#v, err=%v", plan, err)
	}
	if plan.prompt != buildCoopWakeDoorbell("22222222222222222222222222222222") {
		t.Fatalf("re-entered token prompt = %q", plan.prompt)
	}
}

func TestWakeDoorbellStateDeliveredCohortFailsOpenWithoutIdentity(t *testing.T) {
	current := map[string]os.FileInfo{
		"pending.md": wakeIdentityUnavailableFileInfo{name: "pending.md"},
	}
	var state wakeDoorbellState
	state.recordCohortDelivered(current)

	if !state.planCohortDelivery(current) {
		t.Fatal("identity-unavailable delivered cohort was treated as exact")
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
	if deadline, ok := state.nextDeadline(); !ok || !deadline.Equal(now.Add(wakeDoorbellRetryBase)) {
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
	if deadline, ok := state.nextDeadline(); !ok || !deadline.Equal(now.Add(wakeDoorbellRetryBase)) {
		t.Fatalf("re-armed recovery deadline = %s, ok=%v", deadline, ok)
	}
	if !state.planRecoveryAttention(now.Add(wakeDoorbellRetryBase), second) {
		t.Fatal("changed recovery cohort stayed suppressed after rate bound")
	}
	state.recordRecoveryRequired(now.Add(wakeDoorbellRetryBase), second)
	if !sameKnownWakeCohort(state.cohort, second) {
		t.Fatalf("recovery cohort was not advanced: %#v", state.cohort)
	}
	if state.phase == wakeDoorbellCohortDelivered {
		t.Fatal("recovery attention was recorded as cohort delivery")
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
