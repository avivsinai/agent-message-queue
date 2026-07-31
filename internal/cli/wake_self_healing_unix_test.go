//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type scriptedWakeEventWatcher struct {
	events chan fsnotify.Event
	errs   chan error
}

func (watcher *scriptedWakeEventWatcher) Events() <-chan fsnotify.Event {
	return watcher.events
}

func (watcher *scriptedWakeEventWatcher) Errors() <-chan error {
	return watcher.errs
}

func (watcher *scriptedWakeEventWatcher) Close() error {
	return nil
}

func stubFastWakeInboxRetry(t *testing.T) {
	t.Helper()
	originalBase := wakeInboxScanRetryBase
	originalMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = 10 * time.Millisecond
	wakeInboxScanRetryMax = 20 * time.Millisecond
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalBase
		wakeInboxScanRetryMax = originalMax
	})
}

func requireWakePrepared(t *testing.T, cfg wakeConfig, failure string) {
	t.Helper()
	prepared := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan error, 1)
	cfg.controlStop = stop
	cfg.onPrepared = func(wakeAdmissionWatcher) error {
		close(prepared)
		return nil
	}
	go func() {
		done <- runWakeLoop(cfg)
	}()

	select {
	case <-prepared:
	case err := <-done:
		t.Fatalf("%s: %v", failure, err)
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("wake loop stop: %v", err)
	}
}

func TestClassifyWakeFailureDefaultsToRetryAndPreservesFatalDominance(t *testing.T) {
	unknown := errors.New("unclassified effect failure")
	if got := classifyWakeFailure(unknown); got != wakeFailureRetry {
		t.Fatalf("unknown failure disposition = %v, want retry", got)
	}
	if got := classifyWakeFailure(newWakeTerminalForegroundPGRPChangedLoss(1, 2)); got != wakeFailureRetry {
		t.Fatalf("foreground handoff disposition = %v, want retry", got)
	}
	fatal := newWakeTerminalAuthorityLoss("wake generation superseded", nil)
	if got := classifyWakeFailure(errors.Join(unknown, fatal)); got != wakeFailureFatal {
		t.Fatalf("joined ownership loss disposition = %v, want fatal", got)
	}
}

func TestWakeStartupRetryBackoffRemainsWithinCoopReadinessBudget(t *testing.T) {
	got := wakeStartupRetryBackoff(64)
	want := wakeReadyTimeout / 10
	if got != want {
		t.Fatalf("startup retry backoff = %s, want readiness-derived cap %s", got, want)
	}
	if got*2 >= wakeReadyTimeout {
		t.Fatalf("startup retry cap %s leaves too little readiness budget %s", got, wakeReadyTimeout)
	}
}

func TestRunWakeLoopFatalOwnershipLossDominatesJoinedMaintenanceFailure(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	generic := errors.New("status persistence failed")
	fatal := newWakeTerminalAuthorityLoss("wake generation superseded", nil)
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			injectMode:       wakeInjectModeNone,
			controlStop:      make(chan struct{}),
			maintenanceTicks: ticks,
			preconditionCheck: func(*wakeConfig) error {
				return errors.Join(generic, fatal)
			},
		})
	}()

	ticks <- time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, generic) || !errors.Is(err, fatal) {
			t.Fatalf("fatal joined maintenance error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proven ownership loss did not terminate wake loop")
	}
}

func TestRunWakeLoopRetriesTransientBaselineWatcherFailureBeforeReadiness(t *testing.T) {
	stubFastWakeInboxRetry(t)
	originalFactory := newWakeBaselineEventWatcher
	var calls atomic.Int64
	newWakeBaselineEventWatcher = func(path string) (wakeEventWatcher, error) {
		if calls.Add(1) == 1 {
			errs := make(chan error, 1)
			errs <- errors.New("temporary baseline watcher overflow")
			return &scriptedWakeEventWatcher{
				events: make(chan fsnotify.Event),
				errs:   errs,
			}, nil
		}
		return originalFactory(path)
	}
	t.Cleanup(func() {
		newWakeBaselineEventWatcher = originalFactory
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	requireWakePrepared(t, wakeConfig{
		root:              root,
		me:                "codex",
		injectMode:        wakeInjectModeNone,
		baselineRequested: true,
	}, "transient baseline watcher did not recover before readiness")
	if got := calls.Load(); got < 2 {
		t.Fatalf("baseline watcher factory calls = %d, want rearm", got)
	}
}

func TestRunWakeLoopRetriesTransientMainWatcherCreationBeforeReadiness(t *testing.T) {
	stubFastWakeInboxRetry(t)
	originalFactory := newWakeInboxEventWatcher
	var calls atomic.Int64
	newWakeInboxEventWatcher = func(inbox *wakeInboxDir) (wakeEventWatcher, error) {
		if calls.Add(1) == 1 {
			return nil, syscall.EMFILE
		}
		return originalFactory(inbox)
	}
	t.Cleanup(func() {
		newWakeInboxEventWatcher = originalFactory
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	requireWakePrepared(t, wakeConfig{
		root:       root,
		me:         "codex",
		injectMode: wakeInjectModeNone,
	}, "transient main watcher creation did not recover before readiness")
	if got := calls.Load(); got < 2 {
		t.Fatalf("main watcher factory calls = %d, want retry", got)
	}
}

func TestRunWakeLoopRearmsFailedMainWatcherBeforeReadiness(t *testing.T) {
	originalFactory := newWakeInboxEventWatcher
	var calls atomic.Int64
	newWakeInboxEventWatcher = func(inbox *wakeInboxDir) (wakeEventWatcher, error) {
		if calls.Add(1) == 1 {
			errs := make(chan error, 1)
			errs <- errors.New("main watcher failed before admission")
			return &scriptedWakeEventWatcher{
				events: make(chan fsnotify.Event),
				errs:   errs,
			}, nil
		}
		return originalFactory(inbox)
	}
	t.Cleanup(func() {
		newWakeInboxEventWatcher = originalFactory
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	requireWakePrepared(t, wakeConfig{
		root:       root,
		me:         "codex",
		injectMode: wakeInjectModeNone,
	}, "failed main watcher was not rearmed before readiness")
	if got := calls.Load(); got < 2 {
		t.Fatalf("main watcher factory calls = %d, want rearm", got)
	}
}

func TestRunWakeLoopRetriesUnknownMaintenanceFailureByDefault(t *testing.T) {
	originalRead := readTIOCSTILegacySysctl
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		return []byte("1\n"), nil
	}
	t.Cleanup(func() {
		readTIOCSTILegacySysctl = originalRead
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")

	ticks := make(chan time.Time)
	checked := make(chan int, 2)
	stop := make(chan struct{})
	done := make(chan error, 1)
	checks := 0
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:                root,
			me:                  "codex",
			injectMode:          wakeInjectModeRaw,
			requestedInjectMode: wakeInjectModeRaw,
			controlStop:         stop,
			maintenanceTicks:    ticks,
			preconditionCheck: func(cfg *wakeConfig) error {
				checks++
				checked <- checks
				return wakeInjectionPreconditionCheck(
					cfg,
					func() bool { return checks > 1 },
				)
			},
		})
	}()

	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	ticks <- time.Now()
	select {
	case got := <-checked:
		if got != 1 {
			t.Fatalf("first maintenance check = %d", got)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before first maintenance check: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not run first maintenance check")
	}

	select {
	case err := <-done:
		t.Fatalf("unknown maintenance failure killed wake loop: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	ticks <- time.Now()
	select {
	case got := <-checked:
		if got != 2 {
			t.Fatalf("recovery maintenance check = %d", got)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before recovery maintenance check: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not retry maintenance after transient failure")
	}
}

func TestRunWakeLoopRetriesPendingNotifierStatusPersistence(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")

	persistErr := errors.New("presence unavailable")
	attempted := make(chan wakeNotifierStatus, 2)
	var calls atomic.Int64
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	cfg := wakeConfig{
		root:             root,
		me:               "codex",
		injectMode:       wakeInjectModeNone,
		controlStop:      stop,
		maintenanceTicks: ticks,
		recordNotifierStatus: func(status, mode, reason string) error {
			attempted <- wakeNotifierStatus{status: status, mode: mode, reason: reason}
			if calls.Add(1) == 1 {
				return persistErr
			}
			return nil
		},
	}
	if err := persistWakeNotifierStatus(
		&cfg,
		wakeInputRecoveryRequiredStatus,
		wakeInjectModeRaw,
		"retry exact status",
	); !errors.Is(err, persistErr) {
		t.Fatalf("initial notifier status persistence = %v, want %v", err, persistErr)
	}
	<-attempted

	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(cfg)
	}()
	ticks <- time.Now()
	select {
	case got := <-attempted:
		want := (wakeNotifierStatus{
			status: wakeInputRecoveryRequiredStatus,
			mode:   wakeInjectModeRaw,
			reason: "retry exact status",
		})
		if got != want {
			t.Fatalf("retried notifier status = %#v, want %#v", got, want)
		}
	case err := <-done:
		t.Fatalf("pending notifier status failure killed wake loop: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("pending notifier status was not retried on maintenance")
	}
	select {
	case err := <-done:
		t.Fatalf("wake loop exited after notifier status recovered: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("wake loop stop: %v", err)
	}
}

func TestRunWakeLoopRearmsRetainedWatcherAfterTransientClosure(t *testing.T) {
	stubFastWakeInboxRetry(t)

	root, agentDir := newWakeInboxCapabilityForTest(t)
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inboxDir.Close() })

	closed := make(chan struct{})
	attention := make(chan string, 2)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:           root,
			me:             "codex",
			session:        "session1",
			injectMode:     wakeInjectModeNone,
			controlStop:    stop,
			retainedAgent:  agentDir,
			retainedInbox:  inboxDir,
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
			onPrepared: func(watcher wakeAdmissionWatcher) error {
				closer, ok := watcher.(interface{ Close() error })
				if !ok {
					return errors.New("wake watcher is not closeable")
				}
				if err := closer.Close(); err != nil {
					return err
				}
				close(closed)
				return nil
			},
		})
	}()

	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	select {
	case <-closed:
	case err := <-done:
		t.Fatalf("wake loop exited before retained watcher closure: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not close retained watcher")
	}
	select {
	case err := <-done:
		t.Fatalf("retained watcher closure killed wake loop: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	deliverWakeWatcherMessageForTest(t, root, "codex", "after-retained-rearm", "claude")
	awaitWakeAttentionFrom(t, attention, done, "claude")
}

func TestRunWakeLoopRetriesUnknownTIOCSTIErrnoWithValidAuthority(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	stop := make(chan struct{})
	authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	injectWakeTerminalFD = func(uintptr, string) error {
		return &tiocstiInjectionError{Err: syscall.EINVAL, Progress: 0}
	}

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverWakeWatcherMessageForTest(t, root, "codex", "unknown-errno", "claude")

	attention := make(chan string, 2)
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:                root,
			me:                  "codex",
			session:             "session1",
			wakeOwner:           &wakeOwner{},
			injectMode:          wakeInjectModeRaw,
			controlStop:         stop,
			beforeTerminalWrite: authority.BeforeWrite,
			terminalWrite:       authority.Inject,
			attentionIsTTY:      func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()

	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	select {
	case output := <-attention:
		if !strings.Contains(output, "from claude") {
			t.Fatalf("unknown-errno attention = %q", output)
		}
	case err := <-done:
		t.Fatalf("unknown TIOCSTI errno killed wake loop before attention: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("unknown TIOCSTI errno emitted no attention")
	}
	select {
	case err := <-done:
		t.Fatalf("unknown TIOCSTI errno killed wake loop: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRunWakeLoopRecoveryProgressStartsFreshDoorbell(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverWakeWatcherMessageForTest(t, root, "codex", "first", "claude")
	deliverWakeWatcherMessageForTest(t, root, "codex", "second", "claude")

	firstPath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "first.md")
	secondPath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "second.md")
	firstInfo, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	cohort := map[string]os.FileInfo{
		"first.md":  firstInfo,
		"second.md": secondInfo,
	}

	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	})
	stubRawInjectSleep(t)

	writes := make(chan string, 8)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:                  root,
			me:                    "codex",
			session:               "session1",
			wakeOwner:             &wakeOwner{},
			debounce:              5 * time.Millisecond,
			injectMode:            wakeInjectModeRaw,
			controlStop:           stop,
			inputRecoveryRequired: true,
			inputDelivery: wakeInputDeliveryState{
				phase:         wakeInputPayloadPending,
				mode:          wakeInjectModeRaw,
				payload:       coopWakeDoorbell,
				acceptedBytes: 7,
			},
			doorbell: wakeDoorbellState{
				phase:       wakeDoorbellRecoveryRequired,
				cohort:      snapshotWakeFileIdentities(cohort),
				attempts:    1,
				nextAttempt: time.Now().Add(time.Hour),
			},
			terminalWrite: func(data string) error {
				writes <- data
				return nil
			},
			attentionIsTTY: func() bool { return false },
		})
	}()

	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	if err := os.Remove(firstPath); err != nil {
		t.Fatal(err)
	}
	select {
	case write := <-writes:
		if write != coopWakeDoorbell {
			t.Fatalf("post-progress write = %q, want fresh complete doorbell", write)
		}
	case err := <-done:
		t.Fatalf("wake loop exited before fresh recovery doorbell: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("consumer progress left wake permanently recovery-required")
	}
}

func TestRunWakeLoopRepromotesRuntimeLegacyTIOCSTIDemotion(t *testing.T) {
	originalRead := readTIOCSTILegacySysctl
	var disabled atomic.Bool
	disabled.Store(true)
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		if disabled.Load() {
			return []byte("0\n"), nil
		}
		return []byte("1\n"), nil
	}
	t.Cleanup(func() {
		readTIOCSTILegacySysctl = originalRead
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	ticks := make(chan time.Time)
	statuses := make(chan string, 4)
	attention := make(chan string, 2)
	writes := make(chan string, 8)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:                root,
			me:                  "codex",
			session:             "session1",
			wakeOwner:           &wakeOwner{},
			debounce:            5 * time.Millisecond,
			injectMode:          wakeInjectModeRaw,
			requestedInjectMode: wakeInjectModeRaw,
			controlStop:         stop,
			maintenanceTicks:    ticks,
			preconditionCheck: func(cfg *wakeConfig) error {
				return wakeInjectionPreconditionCheck(cfg, func() bool { return true })
			},
			recordNotifierStatus: func(status, _, _ string) error {
				statuses <- status
				return nil
			},
			terminalWrite: func(data string) error {
				writes <- data
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()

	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	ticks <- time.Now()
	select {
	case status := <-statuses:
		if status != wakeInjectorUnsupportedStatus {
			t.Fatalf("demotion status = %q", status)
		}
	case err := <-done:
		t.Fatalf("wake loop exited during runtime demotion: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("runtime capability loss did not demote")
	}

	deliverWakeWatcherMessageForTest(t, root, "codex", "while-demoted", "claude")
	select {
	case <-attention:
	case err := <-done:
		t.Fatalf("wake loop exited before demoted attention: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("demoted wake emitted no attention")
	}

	disabled.Store(false)
	ticks <- time.Now()
	select {
	case write := <-writes:
		if write != coopWakeDoorbell {
			t.Fatalf("restored input write = %q, want complete doorbell", write)
		}
	case err := <-done:
		t.Fatalf("wake loop exited during input restoration: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("restored capability did not immediately drive pending cohort")
	}
	select {
	case status := <-statuses:
		if status != "" {
			t.Fatalf("restored notifier status = %q, want clear", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restored capability did not clear notifier status")
	}
}

func TestRunWakeLoopExplicitNoneNeverPromotes(t *testing.T) {
	originalRead := readTIOCSTILegacySysctl
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		return []byte("1\n"), nil
	}
	t.Cleanup(func() {
		readTIOCSTILegacySysctl = originalRead
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverWakeWatcherMessageForTest(t, root, "codex", "explicit-none", "claude")
	ticks := make(chan time.Time)
	attention := make(chan string, 2)
	writes := make(chan string, 1)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:                root,
			me:                  "codex",
			injectMode:          wakeInjectModeNone,
			requestedInjectMode: wakeInjectModeNone,
			controlStop:         stop,
			maintenanceTicks:    ticks,
			preconditionCheck: func(cfg *wakeConfig) error {
				return wakeInjectionPreconditionCheck(cfg, func() bool { return true })
			},
			terminalWrite: func(data string) error {
				writes <- data
				return nil
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()

	select {
	case <-attention:
	case err := <-done:
		t.Fatalf("explicit-none wake exited before attention: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("explicit-none wake emitted no attention")
	}
	ticks <- time.Now()
	select {
	case write := <-writes:
		t.Fatalf("explicit none promoted and wrote %q", write)
	case err := <-done:
		t.Fatalf("explicit-none wake exited on maintenance: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("wake loop stop: %v", err)
	}
}

func TestNotifyNewMessagesEmptyInboxClearsInputRecovery(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	cfg := &wakeConfig{
		root:                  root,
		me:                    "codex",
		injectMode:            wakeInjectModeRaw,
		inputRecoveryRequired: true,
		inputDelivery: wakeInputDeliveryState{
			phase:               wakeInputPayloadPending,
			mode:                wakeInjectModeRaw,
			payload:             coopWakeDoorbell,
			acceptanceUncertain: true,
		},
		doorbell: wakeDoorbellState{
			phase: wakeDoorbellRecoveryRequired,
			cohort: snapshotWakeFileIdentities(
				wakeDoorbellTestFiles(t, "already-drained.md"),
			),
		},
	}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("empty recovery inbox: %v", err)
	}
	if cfg.inputRecoveryRequired || cfg.inputDelivery.pending() ||
		cfg.doorbell.phase != wakeDoorbellIdle {
		t.Fatalf("empty inbox retained recovery state: %#v %#v", cfg.inputDelivery, cfg.doorbell)
	}
}
