package board

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// A source that failed in pass 1 and is disabled in pass 2 keeps its old
// `source_failed` for ever when the disabled result carries no observation
// time: the store holds the older result because the new one looks older.
// Every result of a pass carries the time of that pass, so pass 2 replaces
// the failure and the floor goes quiet.
func TestAcquireStampsEveryResultWithItsPassTime(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	env := Env{Now: func() time.Time { return now }, LookPath: func(string) (string, error) { return "", nil }}
	failing := failingSource{}
	ctx := context.Background()
	pass1 := Acquire(ctx, env, []Source{failing}, DefaultConfig(), "test")
	input := pass1
	if !input.Results[0].Result.ReadFailed {
		t.Fatal("pass 1 did not record the failed read")
	}
	if !input.Results[0].Result.ObservedAt.Equal(now) {
		t.Fatalf("failed read observed at %v, want the pass time %v", input.Results[0].Result.ObservedAt, now)
	}
	input = Acquire(ctx, env, []Source{Disabled{SourceName: "flaky"}}, DefaultConfig(), "test")
	if input.Results[0].Result.Status != StatusDisabled || input.Results[0].Result.ReadFailed {
		t.Fatalf("pass 2 = %s readFailed=%v, want a disabled healthy result", input.Results[0].Result.Status, input.Results[0].Result.ReadFailed)
	}
	if !input.Results[0].Result.ObservedAt.Equal(now) {
		t.Fatalf("disabled result observed at %v, want the pass time %v", input.Results[0].Result.ObservedAt, now)
	}
	// The store must replace the failed pass-1 result with the disabled
	// pass-2 result, or check keeps reporting source_failed for ever.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.StableTreeIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(base, identity, "host-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	initial, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: pass1}); err != nil {
		t.Fatal(err)
	}
	if err = store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: input}); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Input.Results[0]; got.Result.Status != StatusDisabled || got.Result.ReadFailed || !got.Result.ObservedAt.Equal(now) {
		t.Fatalf("stored result = %s readFailed=%v observedAt=%v; want the disabled pass-2 result", got.Result.Status, got.Result.ReadFailed, got.Result.ObservedAt)
	}
	problems, err := Check(state.Project(DefaultConfig(), now, "test"), DefaultConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if p.Kind == ProblemSourceFailed {
			t.Fatalf("disabled source still reports source_failed: %s", p.Text)
		}
	}
}

type failingSource struct{}

func (failingSource) Name() string                { return "flaky" }
func (failingSource) Discover(Env) (bool, string) { return true, "" }
func (failingSource) Read(context.Context, Env) Result {
	return Result{Status: StatusDegraded, ReadFailed: true, Detail: "read failed", ObservedAt: time.Time{}}
}
