package board

import (
	"context"
	"testing"
	"time"
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
	input := Acquire(context.Background(), env, []Source{failing}, DefaultConfig(), "test")
	if !input.Results[0].Result.ReadFailed {
		t.Fatal("pass 1 did not record the failed read")
	}
	if !input.Results[0].Result.ObservedAt.Equal(now) {
		t.Fatalf("failed read observed at %v, want the pass time %v", input.Results[0].Result.ObservedAt, now)
	}
	input = Acquire(context.Background(), env, []Source{Disabled{SourceName: "flaky"}}, DefaultConfig(), "test")
	if input.Results[0].Result.Status != StatusDisabled || input.Results[0].Result.ReadFailed {
		t.Fatalf("pass 2 = %s readFailed=%v, want a disabled healthy result", input.Results[0].Result.Status, input.Results[0].Result.ReadFailed)
	}
	if !input.Results[0].Result.ObservedAt.Equal(now) {
		t.Fatalf("disabled result observed at %v, want the pass time %v", input.Results[0].Result.ObservedAt, now)
	}
}

type failingSource struct{}

func (failingSource) Name() string                { return "flaky" }
func (failingSource) Discover(Env) (bool, string) { return true, "" }
func (failingSource) Read(context.Context, Env) Result {
	return Result{Status: StatusDegraded, ReadFailed: true, Detail: "read failed", ObservedAt: time.Time{}}
}
