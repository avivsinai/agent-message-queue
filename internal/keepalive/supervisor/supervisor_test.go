package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

type fakeWake struct {
	repairCalls int
	starts      []amq.StartWakeRequest
	startErr    error
}

func (f *fakeWake) RepairWake(ctx context.Context, root, me string) (amq.WakeRepairResult, error) {
	f.repairCalls++
	return amq.WakeRepairResult{Status: "repaired", Reason: "would restore persisted target"}, nil
}

func (f *fakeWake) StartWake(ctx context.Context, req amq.StartWakeRequest) error {
	f.starts = append(f.starts, req)
	return f.startErr
}

type probeAdapter struct {
	err error
}

func (p probeAdapter) Probe(ctx context.Context, target string) error {
	return p.err
}

func TestReconcileEnsuresRegistryTarget(t *testing.T) {
	now := fixedNow()
	wake := &fakeWake{}

	updated, result := testReconciler(wake, probeAdapter{}, now).Reconcile(context.Background(), testEntry())

	if result.Action != ActionEnsured {
		t.Fatalf("action = %q, want %q", result.Action, ActionEnsured)
	}
	if len(wake.starts) != 1 {
		t.Fatalf("ensure calls = %d, want 1", len(wake.starts))
	}
	if wake.repairCalls != 0 {
		t.Fatalf("repairCalls = %d, want 0", wake.repairCalls)
	}
	if wake.starts[0].Timeout != 7*time.Second {
		t.Fatalf("start timeout = %s, want 7s", wake.starts[0].Timeout)
	}
	if updated.State != registry.StateActive {
		t.Fatalf("state = %q, want %q", updated.State, registry.StateActive)
	}
	if updated.FailureCount != 0 || !updated.BackoffUntil.IsZero() || updated.LastError != "" {
		t.Fatalf("active entry retained failure data: %+v", updated)
	}
}

func TestReconcileNeverRepairsPersistedTargetBeforeEnsuringRegistryTarget(t *testing.T) {
	now := fixedNow()
	wake := &fakeWake{}
	entry := testEntry()
	entry.Adapter = "cmux"
	entry.Target = "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"

	updated, result := testReconciler(wake, probeAdapter{}, now).Reconcile(context.Background(), entry)

	if wake.repairCalls != 0 {
		t.Fatalf("repairCalls = %d, want 0; repair could resurrect the persisted Ghostty target", wake.repairCalls)
	}
	if len(wake.starts) != 1 || wake.starts[0].Adapter != "cmux" || wake.starts[0].Target != entry.Target {
		t.Fatalf("ensure requests = %#v, want exact registry cmux target", wake.starts)
	}
	if result.Action != ActionEnsured || updated.State != registry.StateActive {
		t.Fatalf("result = %+v updated = %+v, want ensured active target", result, updated)
	}
}

func TestWrongExistingTargetFailsClosedInsteadOfMarkingActive(t *testing.T) {
	now := fixedNow()
	wake := &fakeWake{
		startErr: errors.New("existing wake inject-via target does not match requested target"),
	}
	entry := testEntry()
	entry.Adapter = "cmux"
	entry.Target = "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3"

	updated, result := testReconciler(wake, probeAdapter{}, now).Reconcile(context.Background(), entry)

	if result.Action != ActionStartFailed {
		t.Fatalf("action = %q, want %q", result.Action, ActionStartFailed)
	}
	if result.Error == nil || !strings.Contains(result.Error.Error(), "does not match") {
		t.Fatalf("error = %v, want target mismatch", result.Error)
	}
	if updated.State == registry.StateActive {
		t.Fatalf("state = %q, must not mark wrong target active", updated.State)
	}
	if updated.LastError == "" || updated.FailureCount != 1 {
		t.Fatalf("failure state not persisted: %+v", updated)
	}
}

func TestStartFreshStartsWithoutRepair(t *testing.T) {
	now := fixedNow()
	wake := &fakeWake{}

	updated, result := testReconciler(wake, probeAdapter{}, now).StartFresh(context.Background(), testEntry())

	if wake.repairCalls != 0 {
		t.Fatalf("repairCalls = %d, want 0", wake.repairCalls)
	}
	if len(wake.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(wake.starts))
	}
	if result.Action != ActionEnsured {
		t.Fatalf("action = %q, want %q", result.Action, ActionEnsured)
	}
	if updated.State != registry.StateActive {
		t.Fatalf("state = %q, want %q", updated.State, registry.StateActive)
	}
}

func TestStartFreshDoesNotAcceptAlreadyRunningAsSuccess(t *testing.T) {
	now := fixedNow()
	wake := &fakeWake{startErr: amq.ErrAlreadyRunning}

	updated, result := testReconciler(wake, probeAdapter{}, now).StartFresh(context.Background(), testEntry())

	if result.Action != ActionStartFailed {
		t.Fatalf("action = %q, want %q", result.Action, ActionStartFailed)
	}
	if !errors.Is(result.Error, amq.ErrAlreadyRunning) {
		t.Fatalf("error = %v, want ErrAlreadyRunning", result.Error)
	}
	if updated.State != registry.StateAttached {
		t.Fatalf("state = %q, want %q", updated.State, registry.StateAttached)
	}
	if updated.LastSupervisorDecision != ActionStartFailed {
		t.Fatalf("LastSupervisorDecision = %q, want %q", updated.LastSupervisorDecision, ActionStartFailed)
	}
}

func TestUnverifiedWakeEnsureBacksOffWithoutRepair(t *testing.T) {
	now := fixedNow()
	wake := &fakeWake{startErr: errors.New("wake lock is unverified; refusing second injector")}

	reconciler := testReconciler(wake, probeAdapter{}, now)
	reconciler.Jitter = func(delay time.Duration) time.Duration { return delay + delay/10 }
	updated, result := reconciler.Reconcile(context.Background(), testEntry())

	if result.Action != ActionStartFailed {
		t.Fatalf("action = %q, want %q", result.Action, ActionStartFailed)
	}
	if len(wake.starts) != 1 {
		t.Fatalf("ensure calls = %d, want 1", len(wake.starts))
	}
	if wake.repairCalls != 0 {
		t.Fatalf("repairCalls = %d, want 0", wake.repairCalls)
	}
	if updated.State != registry.StateAttached {
		t.Fatalf("state = %q, want %q", updated.State, registry.StateAttached)
	}
	if updated.FailureCount != 1 {
		t.Fatalf("FailureCount = %d, want 1", updated.FailureCount)
	}
	if !updated.BackoffUntil.After(now) {
		t.Fatalf("BackoffUntil = %v, want after %v", updated.BackoffUntil, now)
	}
	if got, want := updated.BackoffUntil.Sub(now), 1100*time.Millisecond; got != want {
		t.Fatalf("BackoffUntil-now = %v, want %v", got, want)
	}
}

func TestDetachedTargetDoesNotTouchAMQ(t *testing.T) {
	now := fixedNow()
	wake := &fakeWake{}

	updated, result := testReconciler(wake, probeAdapter{err: errors.New("target gone")}, now).Reconcile(context.Background(), testEntry())

	if result.Action != ActionDetached {
		t.Fatalf("action = %q, want %q", result.Action, ActionDetached)
	}
	if result.AMQTouched {
		t.Fatal("AMQTouched = true, want false")
	}
	if len(wake.starts) != 0 {
		t.Fatalf("starts = %d, want 0", len(wake.starts))
	}
	if updated.State != registry.StateDetached {
		t.Fatalf("state = %q, want %q", updated.State, registry.StateDetached)
	}
}

func TestRepeatedReconcileUsesTargetAwareEnsure(t *testing.T) {
	now := fixedNow()
	wake := &fakeWake{}
	reconciler := testReconciler(wake, probeAdapter{}, now)

	updated, result := reconciler.Reconcile(context.Background(), testEntry())
	if result.Action != ActionEnsured {
		t.Fatalf("first action = %q, want %q", result.Action, ActionEnsured)
	}
	updated, result = reconciler.Reconcile(context.Background(), updated)

	if result.Action != ActionEnsured {
		t.Fatalf("second action = %q, want %q", result.Action, ActionEnsured)
	}
	if len(wake.starts) != 2 {
		t.Fatalf("wake assertions = %d, want one exact-target ensure per pass", len(wake.starts))
	}
	if wake.starts[0].Target != wake.starts[1].Target || wake.starts[0].Adapter != wake.starts[1].Adapter {
		t.Fatalf("ensure requests drifted: %#v", wake.starts)
	}
	if updated.State != registry.StateActive {
		t.Fatalf("state = %q, want %q", updated.State, registry.StateActive)
	}
}

func TestNilWakeBackoffDoesNotReportAMQTouched(t *testing.T) {
	now := fixedNow()

	updated, result := Reconciler{
		Adapter:     probeAdapter{},
		Now:         func() time.Time { return now },
		BackoffBase: time.Second,
		Jitter:      func(delay time.Duration) time.Duration { return delay },
	}.Reconcile(context.Background(), testEntry())

	if result.Action != ActionBackoff {
		t.Fatalf("action = %q, want %q", result.Action, ActionBackoff)
	}
	if result.AMQTouched {
		t.Fatal("AMQTouched = true, want false")
	}
	if updated.State != registry.StateAttached {
		t.Fatalf("state = %q, want %q", updated.State, registry.StateAttached)
	}
}

func testReconciler(wake *fakeWake, adapter probeAdapter, now time.Time) Reconciler {
	return Reconciler{
		Wake:        wake,
		Adapter:     adapter,
		Now:         func() time.Time { return now },
		BackoffBase: time.Second,
		Jitter:      func(delay time.Duration) time.Duration { return delay },
		InjectVia:   "/bin/amq-keepalive",
		WakeTimeout: 7 * time.Second,
	}
}

func testEntry() registry.Entry {
	return registry.Entry{
		ID:      "entry-1",
		Root:    "/tmp/amq-root",
		Agent:   "codex",
		Adapter: "file",
		Target:  "/tmp/inbox.txt",
		State:   registry.StateAttached,
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 6, 26, 15, 0, 0, 0, time.UTC)
}
