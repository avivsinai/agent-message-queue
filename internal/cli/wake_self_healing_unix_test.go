//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
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
	fatal := newWakeTerminalAuthorityLoss("wake generation superseded")
	if got := classifyWakeFailure(errors.Join(unknown, fatal)); got != wakeFailureFatal {
		t.Fatalf("joined ownership loss disposition = %v, want fatal", got)
	}
}

func TestWakeTerminalAuthorityLossReasonCannotSmuggleTransientErrno(t *testing.T) {
	loss := newWakeTerminalAuthorityLoss(
		fmt.Sprintf("wake generation inspection failed: %v", syscall.EMFILE),
	)
	if errors.Is(loss, syscall.EMFILE) {
		t.Fatalf("reason-only authority loss unexpectedly unwraps EMFILE: %v", loss)
	}
	if got := classifyWakeFailure(loss); got != wakeFailureFatal {
		t.Fatalf("reason-only authority loss disposition = %v, want fatal", got)
	}
}

func TestWakeOwnershipLossReasonCannotSmuggleTransientErrno(t *testing.T) {
	loss := newWakeOwnershipLoss(
		fmt.Sprintf("wake owner inspection failed: %v", syscall.ESRCH),
	)
	if errors.Is(loss, syscall.ESRCH) {
		t.Fatalf("reason-only ownership loss unexpectedly unwraps ESRCH: %v", loss)
	}
	if got := classifyWakeFailure(loss); got != wakeFailureFatal {
		t.Fatalf("reason-only ownership loss disposition = %v, want fatal", got)
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
	fatal := newWakeTerminalAuthorityLoss("wake generation superseded")
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

func TestRunWakeLoopTerminatesOnProvenTerminalIdentityLoss(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverWakeWatcherMessageForTest(t, root, "codex", "fatal-terminal-loss", "claude")

	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			injectMode:  wakeInjectModeRaw,
			controlStop: make(chan struct{}),
			terminalWrite: func(string) error {
				return newWakeTerminalAuthorityLoss(
					"retained controlling-terminal identity changed",
				)
			},
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				return len(data), nil
			},
		})
	}()

	select {
	case err := <-done:
		if !isWakeTerminalAuthorityLoss(err) ||
			classifyWakeFailure(err) != wakeFailureFatal {
			t.Fatalf("loop exit = %v, want fatal terminal authority loss", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proven terminal identity loss did not terminate wake loop")
	}
}

func TestRunWakeLoopTerminatesOnOwnerESRCH(t *testing.T) {
	const ownerPID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != ownerPID {
			t.Fatalf("inspected owner pid = %d, want %d", pid, ownerPID)
		}
		return wakeProcessInfo{
			PID:          pid,
			Running:      false,
			InspectError: syscall.ESRCH,
		}
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:             root,
			me:               "codex",
			injectMode:       wakeInjectModeRaw,
			injectVia:        "/unused/test-injector",
			wakeOwner:        &wakeOwner{PID: ownerPID},
			controlStop:      make(chan struct{}),
			maintenanceTicks: ticks,
		})
	}()

	ticks <- time.Now()
	select {
	case err := <-done:
		if classifyWakeFailure(err) != wakeFailureFatal ||
			!strings.Contains(err.Error(), "owner pid 4242 is not running") {
			t.Fatalf("owner ESRCH loop exit = %v, want fatal owner death", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proven owner ESRCH did not terminate wake loop")
	}
}

func TestRunWakeLoopTerminatesOnConclusiveGenerationLoss(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*wakeLockInspection)
		wantReason string
	}{
		{
			name: "missing lock",
			mutate: func(current *wakeLockInspection) {
				current.Exists = false
				current.Status = wakeLockMissing
				current.Lock = wakeLock{}
				current.fileInfo = nil
			},
			wantReason: "wake generation disappeared",
		},
		{
			name: "positive generation mismatch",
			mutate: func(current *wakeLockInspection) {
				current.Lock.Generation = "replacement-generation"
			},
			wantReason: "wake generation changed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := installWakeTerminalAuthorityFixture(t)
			stop := make(chan struct{})
			authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = authority.Close() })
			test.mutate(&fixture.current)

			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			deliverWakeWatcherMessageForTest(
				t,
				root,
				"codex",
				"conclusive-generation-loss",
				"claude",
			)
			done := make(chan error, 1)
			go func() {
				done <- runWakeLoop(wakeConfig{
					root:                root,
					me:                  "codex",
					session:             "session1",
					injectMode:          wakeInjectModeRaw,
					controlStop:         stop,
					beforeTerminalWrite: authority.BeforeWrite,
					terminalWrite:       authority.Inject,
					attentionIsTTY:      func() bool { return false },
					attentionWrite: func(data []byte) (int, error) {
						return len(data), nil
					},
				})
			}()

			select {
			case err := <-done:
				if !isWakeTerminalAuthorityLoss(err) ||
					classifyWakeFailure(err) != wakeFailureFatal ||
					!strings.Contains(err.Error(), test.wantReason) {
					t.Fatalf("conclusive generation loop exit = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s did not terminate wake loop", test.name)
			}
			if len(fixture.injections) != 0 {
				t.Fatalf("%s injected terminal input: %#v", test.name, fixture.injections)
			}
		})
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

func TestRunWakeLoopRetriesTransientCanonicalAgentValidationWhileRearming(t *testing.T) {
	stubFastWakeInboxRetry(t)

	root, agentDir := newWakeInboxCapabilityForTest(t)
	parentPath := filepath.Dir(agentDir.path)
	parentInfo, err := os.Stat(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	parentMode := parentInfo.Mode().Perm()
	t.Cleanup(func() {
		if err := os.Chmod(parentPath, parentMode); err != nil {
			t.Errorf("restore wake agent parent permissions: %v", err)
		}
	})

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
			attentionIsTTY: func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
			onPrepared: func(watcher wakeAdmissionWatcher) error {
				if err := os.Chmod(parentPath, 0); err != nil {
					return err
				}
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

	loopFinished := false
	t.Cleanup(func() {
		if loopFinished {
			return
		}
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
		loopFinished = true
		t.Fatalf("wake loop exited before transient canonical validation: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not close watcher for canonical validation")
	}
	time.Sleep(50 * time.Millisecond)
	if err := os.Chmod(parentPath, parentMode); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		loopFinished = true
		t.Fatalf("transient canonical validation killed wake loop: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	deliverWakeWatcherMessageForTest(t, root, "codex", "after-canonical-retry", "claude")
	awaitWakeAttentionFrom(t, attention, done, "claude")

	close(stop)
	if err := <-done; err != nil {
		loopFinished = true
		t.Fatalf("wake loop stop: %v", err)
	}
	loopFinished = true
}

func TestCanonicalWakeAgentMissingPathRetriesUntilRestored(t *testing.T) {
	_, agentDir := newWakeInboxCapabilityForTest(t)
	movedPath := agentDir.path + ".temporarily-missing"
	if err := os.Rename(agentDir.path, movedPath); err != nil {
		t.Fatal(err)
	}
	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		if err := os.Rename(movedPath, agentDir.path); err != nil {
			t.Errorf("restore canonical wake agent directory: %v", err)
		}
	})

	err := validateCanonicalWakeAgentDir(agentDir)
	if err == nil ||
		!errors.Is(err, syscall.ENOENT) ||
		classifyWakeFailure(err) != wakeFailureRetry {
		t.Fatalf("missing canonical wake agent = %T %v, want retryable ENOENT", err, err)
	}
	var ownershipLoss *wakeOwnershipLossError
	if errors.As(err, &ownershipLoss) {
		t.Fatalf("missing canonical wake agent became ownership loss: %v", err)
	}

	if err := os.Rename(movedPath, agentDir.path); err != nil {
		t.Fatal(err)
	}
	restored = true
	if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
		t.Fatalf("restored canonical wake agent did not self-heal: %v", err)
	}
}

func TestRunWakeLoopRetriesTransientWakeGenerationReadFailure(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	stop := make(chan struct{})
	authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	var inspections atomic.Int64
	var allowRecovery atomic.Bool
	failureObserved := make(chan struct{}, 1)
	inspectWakeTerminalGeneration = func(root, agent string) wakeLockInspection {
		return inspectWakeLockWithReader(
			root,
			agent,
			fixture.generation.LockPath,
			func() ([]byte, os.FileInfo, error) {
				inspections.Add(1)
				if !allowRecovery.Load() {
					select {
					case failureObserved <- struct{}{}:
					default:
					}
					return nil, nil, syscall.EMFILE
				}
				return readWakeLockFileWithInfo(fixture.generation.LockPath)
			},
		)
	}
	injected := make(chan struct{}, 1)
	injectWakeTerminalFD = func(uintptr, string) error {
		select {
		case injected <- struct{}{}:
		default:
		}
		return nil
	}

	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	})
	stubRawInjectSleep(t)

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverWakeWatcherMessageForTest(t, root, "codex", "transient-generation-read", "claude")
	done := make(chan error, 1)
	ticks := make(chan time.Time)
	maintenanceObserved := make(chan struct{}, 8)
	maintenanceRelease := make(chan struct{})
	statuses := make(chan wakeNotifierStatus, 4)
	attention := make(chan string, 8)
	var maintenanceInspections atomic.Int64
	var deliveryValidations atomic.Int64
	var doorbellNow atomic.Int64
	start := time.Now()
	doorbellNow.Store(start.UnixNano())
	loopFinished := false
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			injectMode:  wakeInjectModeRaw,
			controlStop: stop,
			beforeTerminalWrite: func() error {
				deliveryValidations.Add(1)
				return authority.BeforeWrite()
			},
			terminalWrite:    authority.Inject,
			maintenanceTicks: ticks,
			doorbellNow: func() time.Time {
				return time.Unix(0, doorbellNow.Load())
			},
			inspectTerminalGeneration: func() wakeLockInspection {
				maintenanceInspections.Add(1)
				return inspectWakeTerminalGeneration(
					fixture.generation.Root,
					fixture.generation.Agent,
				)
			},
			preconditionCheck: func(*wakeConfig) error {
				maintenanceObserved <- struct{}{}
				select {
				case <-maintenanceRelease:
				case <-stop:
				}
				return nil
			},
			recordNotifierStatus: func(status, mode, reason string) error {
				statuses <- wakeNotifierStatus{status: status, mode: mode, reason: reason}
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
		if loopFinished {
			return
		}
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
	case <-failureObserved:
	case err := <-done:
		loopFinished = true
		t.Fatalf("wake loop exited before transient generation-read failure: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not exercise transient generation-read failure")
	}
	if got := maintenanceInspections.Load(); got != 0 {
		t.Fatalf("delivery attempt performed %d maintenance inspections, want 0", got)
	}

	for observation := 1; observation < wakeUnreadableGenerationNoticeThreshold; observation++ {
		doorbellNow.Store(
			start.Add(time.Duration(observation) * wakeDoorbellAttentionRetryMax).UnixNano(),
		)
		ticks <- time.Now()
		select {
		case <-maintenanceObserved:
		case err := <-done:
			loopFinished = true
			t.Fatalf("transient generation-read failure killed wake loop: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("wake loop did not complete pre-threshold maintenance")
		}
		if observation == wakeUnreadableGenerationNoticeThreshold-1 {
			select {
			case status := <-statuses:
				t.Fatalf(
					"unreadable lock reported after %d maintenance observations: %#v",
					observation,
					status,
				)
			default:
			}
		}
		maintenanceRelease <- struct{}{}
	}

	doorbellNow.Store(
		start.Add(wakeUnreadableGenerationNoticeThreshold * wakeDoorbellAttentionRetryMax).UnixNano(),
	)
	ticks <- time.Now()
	select {
	case <-maintenanceObserved:
	case err := <-done:
		loopFinished = true
		t.Fatalf("transient generation-read failure killed wake loop: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not begin threshold maintenance")
	}
	select {
	case status := <-statuses:
		t.Fatalf(
			"unreadable lock reported before %d maintenance observations: %#v",
			wakeUnreadableGenerationNoticeThreshold,
			status,
		)
	default:
	}
	maintenanceRelease <- struct{}{}

	select {
	case status := <-statuses:
		want := (wakeNotifierStatus{
			status: "degraded",
			mode:   wakeInjectModeRaw,
			reason: "wake lock unreadable",
		})
		if status != want {
			t.Fatalf("unreadable-lock status = %#v, want %#v", status, want)
		}
	case err := <-done:
		loopFinished = true
		t.Fatalf("wake loop exited before unreadable-lock status: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("five maintenance observations emitted no unreadable-lock status")
	}
	if got := maintenanceInspections.Load(); got != wakeUnreadableGenerationNoticeThreshold {
		t.Fatalf(
			"maintenance inspections at status = %d, want %d",
			got,
			wakeUnreadableGenerationNoticeThreshold,
		)
	}
	for {
		select {
		case output := <-attention:
			if strings.Contains(
				output,
				"wake lock unreadable; injection paused; will resume automatically",
			) {
				goto attentionObserved
			}
		case err := <-done:
			loopFinished = true
			t.Fatalf("wake loop exited before unreadable-lock attention: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("five maintenance observations emitted no unreadable-lock attention")
		}
	}

attentionObserved:
	allowRecovery.Store(true)
	doorbellNow.Store(
		start.Add((wakeUnreadableGenerationNoticeThreshold + 1) * wakeDoorbellAttentionRetryMax).UnixNano(),
	)
	select {
	case ticks <- time.Now():
	case err := <-done:
		loopFinished = true
		t.Fatalf("transient generation-read failure killed wake loop: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not accept maintenance retry")
	}
	select {
	case <-injected:
	case err := <-done:
		loopFinished = true
		t.Fatalf("transient generation-read failure killed wake loop: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatalf(
			"wake loop did not retry after transient generation-read failure (delivery validations=%d, inspections=%d, maintenance inspections=%d)",
			deliveryValidations.Load(),
			inspections.Load(),
			maintenanceInspections.Load(),
		)
	}
	select {
	case <-maintenanceObserved:
	case err := <-done:
		loopFinished = true
		t.Fatalf("wake loop exited before recovery maintenance observation: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not reach recovery maintenance observation")
	}
	maintenanceRelease <- struct{}{}
	select {
	case status := <-statuses:
		if status.status != "" || status.reason != "" {
			t.Fatalf("recovered unreadable-lock status = %#v, want clear", status)
		}
	case err := <-done:
		loopFinished = true
		t.Fatalf("wake loop exited before clearing unreadable-lock status: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("same-generation validation did not clear unreadable-lock status")
	}
	if got := maintenanceInspections.Load(); got != wakeUnreadableGenerationNoticeThreshold+1 {
		t.Fatalf(
			"maintenance inspections after recovery = %d, want %d",
			got,
			wakeUnreadableGenerationNoticeThreshold+1,
		)
	}
	close(stop)
	if err := <-done; err != nil {
		loopFinished = true
		t.Fatalf("wake loop stop: %v", err)
	}
	loopFinished = true
	if got := inspections.Load(); got < 2 {
		t.Fatalf("wake generation inspections = %d, want retry after EMFILE", got)
	}
}

func TestUnreadableGenerationNoticeUsesMaintenanceObservationThreshold(t *testing.T) {
	statuses := make(chan wakeNotifierStatus, 2)
	attention := make(chan string, 1)
	unreadable := true
	cfg := wakeConfig{
		me:         "codex",
		injectMode: wakeInjectModeRaw,
		inspectTerminalGeneration: func() wakeLockInspection {
			inspection := wakeLockInspection{Exists: true}
			if !unreadable {
				inspection.fileInfo = wakeIdentityUnavailableFileInfo{name: ".wake.lock"}
			}
			return inspection
		},
		recordNotifierStatus: func(status, mode, reason string) error {
			statuses <- wakeNotifierStatus{status: status, mode: mode, reason: reason}
			return nil
		},
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) {
			attention <- string(data)
			return len(data), nil
		},
	}
	var state wakeUnreadableGenerationNoticeState

	for observation := 1; observation < wakeUnreadableGenerationNoticeThreshold; observation++ {
		state.observe(&cfg)
		select {
		case status := <-statuses:
			t.Fatalf(
				"pre-threshold unreadable status after %d observations = %#v",
				observation,
				status,
			)
		default:
		}
	}

	state.observe(&cfg)
	select {
	case status := <-statuses:
		want := (wakeNotifierStatus{
			status: "degraded",
			mode:   wakeInjectModeRaw,
			reason: "wake lock unreadable",
		})
		if status != want {
			t.Fatalf("threshold unreadable status = %#v, want %#v", status, want)
		}
	default:
		t.Fatal("maintenance threshold did not activate unreadable-lock status")
	}
	select {
	case output := <-attention:
		if !strings.Contains(
			output,
			"wake lock unreadable; injection paused; will resume automatically",
		) {
			t.Fatalf("threshold unreadable attention = %q", output)
		}
	default:
		t.Fatal("maintenance threshold did not emit unreadable-lock attention")
	}

	unreadable = false
	state.observe(&cfg)
	select {
	case status := <-statuses:
		if status.status != "" || status.reason != "" {
			t.Fatalf("recovered unreadable status = %#v, want clear", status)
		}
	default:
		t.Fatal("readable validation did not clear degraded status")
	}
}

func TestUnreadableGenerationNoticeRetriesFailedStatusWrites(t *testing.T) {
	unreadable := true
	setFailures := 1
	clearFailures := 1
	var statusAttempts []wakeNotifierStatus
	cfg := wakeConfig{
		me:         "codex",
		injectMode: wakeInjectModeRaw,
		inspectTerminalGeneration: func() wakeLockInspection {
			inspection := wakeLockInspection{Exists: true}
			if !unreadable {
				inspection.fileInfo = wakeIdentityUnavailableFileInfo{name: ".wake.lock"}
			}
			return inspection
		},
		recordNotifierStatus: func(status, mode, reason string) error {
			statusAttempts = append(statusAttempts, wakeNotifierStatus{
				status: status,
				mode:   mode,
				reason: reason,
			})
			if status != "" && setFailures > 0 {
				setFailures--
				return errors.New("temporary degraded-status write failure")
			}
			if status == "" && clearFailures > 0 {
				clearFailures--
				return errors.New("temporary status-clear failure")
			}
			return nil
		},
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) {
			return len(data), nil
		},
		diagnosticIsTTY: func() bool { return false },
	}
	var state wakeUnreadableGenerationNoticeState

	for range wakeUnreadableGenerationNoticeThreshold {
		state.observe(&cfg)
	}
	if state.statusActive {
		t.Fatal("failed degraded-status write was recorded as active")
	}
	state.observe(&cfg)
	if !state.statusActive {
		t.Fatal("degraded status was not retried after a transient write failure")
	}

	unreadable = false
	state.observe(&cfg)
	if !state.statusActive {
		t.Fatal("failed status clear was recorded as successful")
	}
	state.observe(&cfg)
	if state.statusActive {
		t.Fatal("status clear was not retried after a transient write failure")
	}

	if got, want := len(statusAttempts), 4; got != want {
		t.Fatalf("status write attempts = %d, want %d", got, want)
	}
}

func TestUnreadableGenerationNoticeRelinquishesWithoutOverwritingRecoveryStatus(t *testing.T) {
	var inspected atomic.Bool
	cfg := wakeConfig{
		injectMode:            wakeInjectModeRaw,
		inputRecoveryRequired: true,
		inspectTerminalGeneration: func() wakeLockInspection {
			inspected.Store(true)
			return wakeLockInspection{Exists: true}
		},
		recordNotifierStatus: func(_, _, _ string) error {
			t.Fatal("relinquished unreadable-lock observer overwrote recovery status")
			return nil
		},
	}
	state := wakeUnreadableGenerationNoticeState{
		consecutiveFailures: wakeUnreadableGenerationNoticeThreshold - 1,
		statusActive:        true,
		attentionDelivered:  true,
	}

	state.observe(&cfg)

	if inspected.Load() {
		t.Fatal("recovery-owned status performed an unreadable-lock inspection")
	}
	if state != (wakeUnreadableGenerationNoticeState{}) {
		t.Fatalf("relinquished unreadable-lock state = %#v, want reset", state)
	}
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
	maintenanceObserved := make(chan struct{}, 5)
	attention := make(chan string, 2)
	statuses := make(chan string, 1)
	writes := make(chan string, 1)
	var generationReads atomic.Int64
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
			inspectTerminalGeneration: func() wakeLockInspection {
				generationReads.Add(1)
				return wakeLockInspection{Exists: true}
			},
			preconditionCheck: func(cfg *wakeConfig) error {
				maintenanceObserved <- struct{}{}
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

	select {
	case <-attention:
	case err := <-done:
		t.Fatalf("explicit-none wake exited before attention: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("explicit-none wake emitted no attention")
	}
	for range 5 {
		ticks <- time.Now()
		select {
		case <-maintenanceObserved:
		case err := <-done:
			t.Fatalf("explicit-none wake exited on maintenance: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("explicit-none wake did not complete maintenance")
		}
	}
	select {
	case write := <-writes:
		t.Fatalf("explicit none promoted and wrote %q", write)
	case status := <-statuses:
		t.Fatalf("explicit none reported unreadable-lock status %q", status)
	case err := <-done:
		t.Fatalf("explicit-none wake exited on maintenance: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if got := generationReads.Load(); got != 0 {
		t.Fatalf("explicit none performed %d terminal-generation reads, want 0", got)
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
