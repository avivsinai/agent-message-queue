//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

func stubSignalWakeProcess(t *testing.T, fn func(pid int, sig os.Signal) error) {
	t.Helper()
	oldSignal := signalWakeProcess
	oldGrace := wakeTerminateGrace
	signalWakeProcess = fn
	wakeTerminateGrace = 0
	t.Cleanup(func() {
		signalWakeProcess = oldSignal
		wakeTerminateGrace = oldGrace
	})
}

func stubWakeCurrentTTY(t *testing.T, fn func() string) {
	t.Helper()
	old := getWakeCurrentTTY
	getWakeCurrentTTY = fn
	t.Cleanup(func() {
		getWakeCurrentTTY = old
	})
}

func stubWakeProcessSID(t *testing.T, fn func(pid int) (int, error)) {
	t.Helper()
	old := getWakeProcessSID
	getWakeProcessSID = fn
	t.Cleanup(func() {
		getWakeProcessSID = old
	})
}

func writeWakePreparedForTest(t *testing.T, root, me string) {
	t.Helper()
	inspection := inspectWakeLock(root, me)
	if err := writeWakePreparedFile(root, me, inspection); err != nil {
		t.Fatalf("writeWakePreparedFile: %v", err)
	}
}

func stubWakeTTYSupport(t *testing.T) {
	t.Helper()
	oldAvailable := wakeTIOCSTIAvailable
	oldIsTTY := wakeInputIsTTY
	wakeTIOCSTIAvailable = func() bool { return true }
	wakeInputIsTTY = func() bool { return true }
	t.Cleanup(func() {
		wakeTIOCSTIAvailable = oldAvailable
		wakeInputIsTTY = oldIsTTY
	})
}

func writeExecutableForTest(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(secureTempDirForTest(t), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

func TestRunWakeWithLoopInjectViaSkipsTTYStartupRequirement(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	var got wakeConfig
	errDone := errors.New("done")
	injector := writeExecutableForTest(t, "inject tool")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
		"--inject-arg", "Team Alpha",
		"--inject-timeout", "250ms",
	}, func(cfg wakeConfig) error {
		got = cfg
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
	if got.injectVia != injector {
		t.Fatalf("expected inject executable with spaces, got %q", got.injectVia)
	}
	if strings.Join(got.injectArgs, "|") != "exec|Team Alpha" {
		t.Fatalf("expected fixed inject args, got %#v", got.injectArgs)
	}
	if got.injectTimeout != 250*time.Millisecond {
		t.Fatalf("expected inject timeout 250ms, got %s", got.injectTimeout)
	}
}

func TestRunWakeWithLoopWritesReadyFileAfterLock(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
			t.Fatalf("ready file published before wake preparation: %v", statErr)
		}
		if err := cfg.onPrepared(nil); err != nil {
			t.Fatalf("publish readiness: %v", err)
		}
		if _, statErr := os.Stat(readyPath); statErr != nil {
			t.Fatalf("expected ready file after wake preparation: %v", statErr)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopBaselinesBeforeReadiness(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	inboxNew := fsq.AgentInboxNew(root, "orchestrator")
	if err := os.WriteFile(filepath.Join(inboxNew, "stale.md"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale message: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--baseline-existing",
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		if !cfg.baselineRequested {
			t.Fatal("baseline request was not carried into the owned wake loop")
		}
		if cfg.baselineExisting != nil {
			t.Fatal("baseline was captured before watcher setup and wake ownership")
		}
		watcher, watcherErr := fsnotify.NewWatcher()
		if watcherErr != nil {
			t.Fatalf("NewWatcher: %v", watcherErr)
		}
		defer func() { _ = watcher.Close() }()
		if watcherErr := watcher.Add(inboxNew); watcherErr != nil {
			t.Fatalf("watch inbox: %v", watcherErr)
		}
		if prepErr := prepareWakeBaseline(&cfg, watcher, inboxNew); prepErr != nil {
			t.Fatalf("prepareWakeBaseline: %v", prepErr)
		}
		if _, ok := cfg.baselineExisting["stale.md"]; !ok {
			t.Fatal("stale message missing from watcher-armed startup baseline")
		}
		if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
			t.Fatalf("ready file published before baseline preparation: %v", statErr)
		}
		if err := cfg.onPrepared(nil); err != nil {
			t.Fatalf("publish readiness: %v", err)
		}
		if _, statErr := os.Stat(readyPath); statErr != nil {
			t.Fatalf("expected ready file after baseline snapshot: %v", statErr)
		}
		if _, exists, preparedErr := readWakeGenerationFile(wakePreparedPath(root, "orchestrator"), "wake prepared marker"); preparedErr != nil || !exists {
			t.Fatalf("generation-bound prepared marker missing: exists=%v err=%v", exists, preparedErr)
		}
		if err := os.WriteFile(filepath.Join(inboxNew, "fresh.md"), []byte("fresh"), 0o600); err != nil {
			t.Fatalf("write fresh message: %v", err)
		}
		if _, ok := cfg.baselineExisting["fresh.md"]; ok {
			t.Fatal("message arriving after readiness was incorrectly baselined")
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestBaselineFreshMailboxStillStarts(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}

	baseline, err := snapshotWakeExistingMessages(root, "fresh-agent")
	if err != nil {
		t.Fatalf("missing inbox/new should be treated as an empty baseline: %v", err)
	}
	if len(baseline) != 0 {
		t.Fatalf("baseline = %#v, want empty", baseline)
	}
}

func TestPrepareWakeBaselineInvalidatesSameNameReplacementDuringSnapshot(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	inboxNew := fsq.AgentInboxNew(root, "alice")
	samePath := filepath.Join(inboxNew, "same.md")
	if err := os.WriteFile(samePath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write old message: %v", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(inboxNew); err != nil {
		t.Fatalf("watch inbox: %v", err)
	}

	originalInfo := snapshotWakeDirEntryInfo
	replaced := false
	snapshotWakeDirEntryInfo = func(entry os.DirEntry) (os.FileInfo, error) {
		if entry.Name() == "same.md" && !replaced {
			replaced = true
			if err := os.Rename(samePath, filepath.Join(inboxNew, "old.md")); err != nil {
				return nil, err
			}
			if err := os.WriteFile(samePath, []byte("replacement"), 0o600); err != nil {
				return nil, err
			}
		}
		return entry.Info()
	}
	t.Cleanup(func() { snapshotWakeDirEntryInfo = originalInfo })

	cfg := wakeConfig{root: root, me: "alice", baselineRequested: true}
	if err := prepareWakeBaseline(&cfg, watcher, inboxNew); err != nil {
		t.Fatalf("prepareWakeBaseline: %v", err)
	}
	if !replaced {
		t.Fatal("same-name replacement hook did not run")
	}
	if _, ignored := cfg.baselineExisting["same.md"]; ignored {
		t.Fatal("same-name replacement created during snapshot remained baselined")
	}
}

func TestFailWakeOnWatcherErrorClearsBaselineAndExits(t *testing.T) {
	cfg := wakeConfig{baselineExisting: map[string]wakeFileIdentity{"stale.md": {}}}
	cause := errors.New("overflow")
	err := failWakeOnWatcherError(&cfg, "watcher error", cause)
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("watcher failure = %v", err)
	}
	if cfg.baselineExisting != nil {
		t.Fatalf("watcher error retained baseline tombstones: %#v", cfg.baselineExisting)
	}
}

func TestRunWakeLoopStopsBeforePublishingPreparedMarker(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	prepared := false
	err := runWakeLoop(wakeConfig{
		root:        secureTempDirForTest(t),
		me:          "orchestrator",
		controlStop: stop,
		onPrepared: func(wakeAdmissionWatcher) error {
			prepared = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runWakeLoop: %v", err)
	}
	if prepared {
		t.Fatal("prepared marker was published after cooperative stop was already pending")
	}
}

func TestBaselineDLQRetryWithSameFilenameRemainsNotifyEligible(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema: 1, ID: "stale", From: "codex", To: []string{"alice"},
			Thread: "p2p/alice__codex", Subject: "stale", Created: "2026-07-22T00:00:00Z",
		},
		Body: "body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, "alice"), "same.md"), data, 0o600); err != nil {
		t.Fatalf("write message: %v", err)
	}

	cfg, outputPath := injectViaCaptureConfig(t)
	cfg.me = "alice"
	cfg.root = root
	cfg.previewLen = 48
	baseline, err := snapshotWakeExistingMessages(root, "alice")
	if err != nil {
		t.Fatalf("snapshot baseline: %v", err)
	}
	target := mustNewWakeTargetForTest(t, root, "alice", cfg.injectVia, cfg.injectArgs)
	lock := bindWakeLockToTarget(wakeLock{
		Root:       canonicalWakeRoot(root),
		Agent:      "alice",
		Generation: "dlq-retry-generation",
		BootID:     wakeRepairTestBootID,
	}, target)
	floor, err := newWakeRepairFloor(root, "alice", lock, target, baseline)
	if err != nil {
		t.Fatalf("new wake repair floor: %v", err)
	}
	if err := writeWakeRepairFloor(root, "alice", floor); err != nil {
		t.Fatalf("write wake repair floor: %v", err)
	}
	persisted, exists, err := readWakeRepairFloor(root, "alice")
	if err != nil || !exists {
		t.Fatalf("read wake repair floor: exists=%v err=%v", exists, err)
	}
	cfg.baselineExisting = persisted.Existing
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify stale baseline: %v", err)
	}
	if got, err := os.ReadFile(outputPath); err == nil || !os.IsNotExist(err) || len(got) != 0 {
		t.Fatalf("baseline message notified before DLQ retry: bytes=%d err=%v", len(got), err)
	}

	rootIdentity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatalf("snapshot delivery root: %v", err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, rootIdentity)
	if err != nil {
		t.Fatalf("open delivery root: %v", err)
	}
	defer func() { _ = deliveryRoot.Close() }()
	dlqPath, err := fsq.MoveToDLQ(deliveryRoot, "alice", "same.md", "stale", "test", "retry identity")
	if err != nil {
		t.Fatalf("move baseline message to DLQ: %v", err)
	}
	if err := fsq.RetryFromDLQ(deliveryRoot, "alice", filepath.Base(dlqPath), false); err != nil {
		t.Fatalf("retry baseline message from DLQ: %v", err)
	}
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify DLQ retry: %v", err)
	}
	if got, err := os.ReadFile(outputPath); err != nil || len(got) == 0 {
		t.Fatalf("same-name DLQ retry did not notify: bytes=%d err=%v", len(got), err)
	}
}

func TestRunWakeWithLoopNoneSkipsTTYAndWritesReadyFile(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-mode", "none",
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		if cfg.injectMode != wakeInjectModeNone {
			t.Fatalf("injectMode = %q, want none", cfg.injectMode)
		}
		if err := cfg.onPrepared(nil); err != nil {
			t.Fatalf("publish readiness: %v", err)
		}
		if _, statErr := os.Stat(readyPath); statErr != nil {
			t.Fatalf("expected ready file after wake preparation: %v", statErr)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopPersistsEffectiveAutoMode(t *testing.T) {
	stubWakeTTYSupport(t)

	for _, tc := range []struct {
		name string
		me   string
		want string
	}{
		{name: "claude uses raw", me: "claude", want: wakeInjectModeRaw},
		{name: "other handles use paste", me: "grok", want: wakeInjectModePaste},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatalf("EnsureRootDirs: %v", err)
			}
			if err := fsq.EnsureAgentDirs(root, tc.me); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}

			errDone := errors.New("done")
			err := runWakeWithLoop([]string{
				"--root", root,
				"--me", tc.me,
				"--inject-mode", "auto",
			}, func(cfg wakeConfig) error {
				lockPath := filepath.Join(fsq.AgentBase(root, tc.me), ".wake.lock")
				data, readErr := os.ReadFile(lockPath)
				if readErr != nil {
					t.Fatalf("read wake lock: %v", readErr)
				}
				var lock wakeLock
				if unmarshalErr := json.Unmarshal(data, &lock); unmarshalErr != nil {
					t.Fatalf("unmarshal wake lock: %v", unmarshalErr)
				}
				if lock.WakeMode != tc.want {
					t.Fatalf("WakeMode = %q, want %q", lock.WakeMode, tc.want)
				}
				return errDone
			})
			if !errors.Is(err, errDone) {
				t.Fatalf("expected loop sentinel error, got %v", err)
			}
		})
	}
}

func TestRunWakeHelpHidesInternalReadyFlags(t *testing.T) {
	stdout, _, err := captureWakeRepairOutput(t, func() error {
		return runWake([]string{"--help"})
	})
	if err != nil {
		t.Fatalf("runWake --help: %v", err)
	}
	for _, hidden := range []string{"ready-file", "accept-existing-wake", "repair-lineage"} {
		if strings.Contains(stdout, hidden) {
			t.Fatalf("wake help should hide %s:\n%s", hidden, stdout)
		}
	}
	if !strings.Contains(stdout, "inject-cmd") {
		t.Fatalf("wake help should keep --inject-cmd visible:\n%s", stdout)
	}
	if !strings.Contains(stdout, "baseline-existing") {
		t.Fatalf("wake help should keep --baseline-existing visible:\n%s", stdout)
	}
	if !strings.Contains(stdout, "none") || !strings.Contains(stdout, "zero terminal input") {
		t.Fatalf("wake help should document none as zero-input mode:\n%s", stdout)
	}
}

func TestRunWakeWithLoopWritesInjectViaWakeTarget(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		target, exists, targetErr := readWakeTarget(root, "orchestrator")
		if targetErr != nil {
			t.Fatalf("readWakeTarget: %v", targetErr)
		}
		if !exists {
			t.Fatal("expected wake target to be written")
		}
		if target.InjectVia != injector || strings.Join(target.InjectArgs, "|") != "exec" {
			t.Fatalf("unexpected target: %#v", target)
		}
		if target.Owner != nil {
			t.Fatalf("generic inject-via wake target should not record owner: %#v", target.Owner)
		}
		if cfg.wakeOwner != nil {
			t.Fatalf("generic inject-via wake config should not record owner: %#v", cfg.wakeOwner)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopPersistsInjectViaWakeOwnerFromEnv(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	ownerEnv, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatalf("encodeWakeOwnerEnv: %v", err)
	}
	t.Setenv(envWakeOwner, ownerEnv)
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if got != owner {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		return wakeOwnerObservation{State: wakeOwnerSame}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	injector := writeExecutableForTest(t, "injector")
	errDone := errors.New("done")
	err = runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
	}, func(cfg wakeConfig) error {
		if cfg.wakeOwner == nil || *cfg.wakeOwner != owner {
			t.Fatalf("cfg.wakeOwner = %#v, want %#v", cfg.wakeOwner, owner)
		}
		target, exists, targetErr := readWakeTarget(root, "orchestrator")
		if targetErr != nil {
			t.Fatalf("readWakeTarget: %v", targetErr)
		}
		if !exists {
			t.Fatal("expected wake target to be written")
		}
		if target.Owner == nil || *target.Owner != owner {
			t.Fatalf("target.Owner = %#v, want %#v", target.Owner, owner)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopExecutesResolvedInjectViaPath(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	base := secureTempDirForTest(t)
	realDir := filepath.Join(base, "real-bin")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real bin: %v", err)
	}
	injector := filepath.Join(realDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	linkDir := filepath.Join(base, "link-bin")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink bin: %v", err)
	}

	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", filepath.Join(linkDir, "injector"),
	}, func(cfg wakeConfig) error {
		if cfg.injectVia != injector {
			t.Fatalf("cfg.injectVia = %q, want resolved %q", cfg.injectVia, injector)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopPersistsResolvedLeafSymlinkInjectViaPath(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	base := secureTempDirForTest(t)
	cellarDir := filepath.Join(base, "Cellar", "injector", "1.0.0", "bin")
	if err := os.MkdirAll(cellarDir, 0o700); err != nil {
		t.Fatalf("mkdir cellar bin: %v", err)
	}
	injector := filepath.Join(cellarDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	binDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	link := filepath.Join(binDir, "injector")
	if err := os.Symlink(injector, link); err != nil {
		t.Fatalf("symlink injector: %v", err)
	}

	errDone := errors.New("done")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", link,
	}, func(cfg wakeConfig) error {
		if cfg.injectVia != injector {
			t.Fatalf("cfg.injectVia = %q, want resolved %q", cfg.injectVia, injector)
		}
		target, exists, err := readWakeTarget(root, "orchestrator")
		if err != nil || !exists {
			t.Fatalf("readWakeTarget exists=%v err=%v", exists, err)
		}
		if target.InjectVia != injector {
			t.Fatalf("target inject_via = %q, want resolved %q", target.InjectVia, injector)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatalf("expected loop sentinel error, got %v", err)
	}
}

func TestRunWakeWithLoopRejectsUnsafeInjectViaBeforeLoop(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	injector := writeExecutableForTest(t, "injector")
	if err := os.Chmod(injector, 0o777); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	var runErr error
	_ = captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
			"--inject-arg", "exec",
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with unsafe inject_via: %#v", cfg)
			return nil
		})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "group/world-writable") {
		t.Fatalf("expected unsafe inject_via rejection, got %v", runErr)
	}
	if _, exists, targetErr := readWakeTarget(root, "orchestrator"); targetErr != nil || exists {
		t.Fatalf("wake target exists=%v err=%v, want absent with no read error", exists, targetErr)
	}
}

func TestInjectViaRevalidatesExecutableBeforeExec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(t *testing.T, path string)
		wantText string
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				target := writeExecutableForTest(t, "target-injector")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove injector: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink injector: %v", err)
				}
			},
			wantText: "must be a resolved path",
		},
		{
			name: "nonregular",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove injector: %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir injector path: %v", err)
				}
			},
			wantText: "must be a regular file",
		},
		{
			name: "world_writable",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0o777); err != nil {
					t.Fatalf("chmod injector: %v", err)
				}
			},
			wantText: "group/world-writable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			injector := writeExecutableForTest(t, "injector-"+tc.name)
			cfg := &wakeConfig{
				injectVia:     injector,
				injectTimeout: time.Second,
			}
			tc.mutate(t, injector)

			err := injectVia(cfg, "payload")
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("expected %q rejection, got %v", tc.wantText, err)
			}
		})
	}
}

func TestRunWakeWithLoopKeepsOldWakeTargetWhenNewTargetIsUnsafe(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	oldInjector := writeExecutableForTest(t, "old-injector")
	if err := writeWakeTarget(root, "orchestrator", mustNewWakeTargetForTest(t, root, "orchestrator", oldInjector, []string{"old"})); err != nil {
		t.Fatalf("write old wake target: %v", err)
	}
	newInjector := writeExecutableForTest(t, "new-injector")
	if err := os.Chmod(newInjector, 0o777); err != nil {
		t.Fatalf("chmod injector: %v", err)
	}
	var runErr error
	_ = captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", newInjector,
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with unsafe inject_via: %#v", cfg)
			return nil
		})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "group/world-writable") {
		t.Fatalf("expected unsafe inject_via rejection, got %v", runErr)
	}
	target, exists, err := readWakeTarget(root, "orchestrator")
	if err != nil || !exists {
		t.Fatalf("wake target exists=%v err=%v, want old target retained", exists, err)
	}
	if target.InjectVia != oldInjector {
		t.Fatalf("wake target inject_via = %q, want old target %q", target.InjectVia, oldInjector)
	}
}

func TestRunWakeWithLoopRejectsInjectorSwappedAfterTargetWrite(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "injector")
	if err := writeWakeTarget(root, "orchestrator", mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})); err != nil {
		t.Fatalf("write wake target: %v", err)
	}
	if err := os.Remove(injector); err != nil {
		t.Fatalf("remove injector: %v", err)
	}
	nonExecutable := filepath.Join(secureTempDirForTest(t), "non-executable-injector")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write non-executable injector: %v", err)
	}
	if err := os.Symlink(nonExecutable, injector); err != nil {
		t.Fatalf("swap injector to symlink: %v", err)
	}

	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run after injector swap: %#v", cfg)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("expected swapped injector rejection, got %v", err)
	}
}

func TestRunWakeWithLoopDoesNotWriteReadyFileWhenLockBlocked(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected existing wake lock error")
	}
	if !strings.Contains(err.Error(), "wake already running") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopWaitsForCurrentPreparedMarkerPastStaleGeneration(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, nil)
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "generation-1",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	run := func() error {
		return runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
			"--ready-file", readyPath,
			"--accept-existing-wake",
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
			return nil
		})
	}

	if err := writeWakeGenerationFile(wakePreparedPath(root, "orchestrator"), "wake prepared marker", wakeReady{
		Schema:       wakeReadySchema,
		Generation:   "previous-generation",
		TargetDigest: mustWakeTargetDigest(target),
	}); err != nil {
		t.Fatalf("write previous-generation prepared marker: %v", err)
	}
	inspection := inspectWakeLock(root, "orchestrator")
	if err := writeWakeReadyFileForPreparedWake(root, "orchestrator", readyPath, inspection, time.Now().Add(40*time.Millisecond)); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("stale prepared marker deadline error = %v", err)
	}

	retryEntered := make(chan struct{}, 1)
	originalRetry := waitForWakePreparedRetry
	waitForWakePreparedRetry = func(deadline time.Time) bool {
		select {
		case retryEntered <- struct{}{}:
		default:
		}
		return originalRetry(deadline)
	}
	t.Cleanup(func() { waitForWakePreparedRetry = originalRetry })
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case <-retryEntered:
	case err := <-done:
		t.Fatalf("existing wake returned instead of polling past the stale marker: %v", err)
	case <-time.After(time.Second):
		t.Fatal("existing wake never entered prepared-marker polling")
	}
	writeWakePreparedForTest(t, root, "orchestrator")
	if err := <-done; err != nil {
		t.Fatalf("expected existing usable wake to satisfy ready file, got %v", err)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("ready file should exist, statErr=%v", statErr)
	}
	current := inspectWakeLock(root, "orchestrator")
	if err := writeWakeGenerationFile(wakePreparedPath(root, "orchestrator"), "wake prepared marker", wakeReady{
		Schema:       wakeReadySchema,
		Generation:   current.Lock.Generation,
		TargetDigest: "same-generation-wrong-target",
	}); err != nil {
		t.Fatalf("write same-generation invalid marker: %v", err)
	}
	if _, err := validateWakePreparedFileAgainstInspection(root, "orchestrator", current); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("same-generation target mismatch was not rejected: %v", err)
	}
}

func TestRunWakeWithLoopRetriesCreatingWakeUntilPrepared(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	if err := os.WriteFile(lockPath, []byte("{"), 0o600); err != nil {
		t.Fatalf("write creating lock: %v", err)
	}
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, nil)
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	retryEntered := make(chan struct{}, 1)
	originalRetry := waitForWakePreparedRetry
	waitForWakePreparedRetry = func(deadline time.Time) bool {
		select {
		case retryEntered <- struct{}{}:
		default:
		}
		return originalRetry(deadline)
	}
	t.Cleanup(func() { waitForWakePreparedRetry = originalRetry })
	done := make(chan error, 1)
	go func() {
		done <- runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-via", injector,
			"--ready-file", readyPath,
			"--accept-existing-wake",
		}, func(cfg wakeConfig) error {
			t.Errorf("loop should not run with an existing wake: %#v", cfg)
			return nil
		})
	}()
	select {
	case <-retryEntered:
	case err := <-done:
		t.Fatalf("helper returned instead of retrying transient lock creation: %v", err)
	case <-time.After(time.Second):
		t.Fatal("helper never retried transient lock creation")
	}
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "boot-1",
		Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
	}, target))
	writeWakePreparedForTest(t, root, "orchestrator")
	if err := <-done; err != nil {
		t.Fatalf("creating wake did not become reusable: %v", err)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("ready file missing: %v", err)
	}
}

func TestRunWakeWithLoopNoneRejectsExistingInputWake(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-mode", "auto"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "test-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-mode", "none",
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing input wake: %#v", cfg)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "requested --inject-mode none") {
		t.Fatalf("error = %v, want zero-input existing-wake refusal", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopNoneAcceptsExistingNoneWake(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-mode", "none"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeNone,
		Generation:   "generation-1",
	})
	writeWakePreparedForTest(t, root, "orchestrator")
	stalePath := filepath.Join(fsq.AgentInboxNew(root, "orchestrator"), "stale.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale message: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	var err error
	stderr := captureWakeStderr(t, func() {
		err = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-mode", "none",
			"--baseline-existing",
			"--ready-file", readyPath,
			"--accept-existing-wake",
		}, func(cfg wakeConfig) error {
			t.Fatalf("loop should not run with an existing none wake: %#v", cfg)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("expected existing none wake to satisfy readiness, got %v", err)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("ready file should exist, statErr=%v", statErr)
	}
	if !strings.Contains(stderr, "this launch did not re-baseline it, so pending backlog may still notify") {
		t.Fatalf("reuse warning missing from stderr: %q", stderr)
	}
	if _, statErr := os.Stat(stalePath); statErr != nil {
		t.Fatalf("reused wake moved or removed stale backlog: %v", statErr)
	}
}

func TestRequireWakeLockUsableRejectsNoneForNonNone(t *testing.T) {
	inspection := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		IdentityConfirmed: true,
		Agent:             "codex",
		Lock:              wakeLock{WakeMode: wakeInjectModeNone, TTY: "test-tty"},
	}
	if err := requireWakeLockUsable(inspection, wakeInjectModeRaw, nil); err == nil {
		t.Fatal("expected a none wake to be rejected for raw mode")
	}
}

func TestRequireWakeLockUsableRawVsPaste(t *testing.T) {
	inspection := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		IdentityConfirmed: true,
		Agent:             "codex",
		Lock:              wakeLock{WakeMode: wakeInjectModeRaw, TTY: "test-tty"},
	}
	if err := requireWakeLockUsable(inspection, wakeInjectModePaste, nil); err == nil {
		t.Fatal("expected raw and paste wakes to be incompatible")
	}
	if err := requireWakeLockUsable(inspection, wakeInjectModeRaw, nil); err != nil {
		t.Fatalf("expected matching raw wake to be usable: %v", err)
	}
}

func TestRequireWakeLockUsableLegacyModeCompatibility(t *testing.T) {
	inspection := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		IdentityConfirmed: true,
		Agent:             "codex",
		Lock:              wakeLock{TTY: "test-tty"},
	}

	for _, tc := range []struct {
		name         string
		requiredMode string
		wantErr      bool
	}{
		{name: "raw accepted", requiredMode: wakeInjectModeRaw},
		{name: "paste accepted", requiredMode: wakeInjectModePaste},
		{name: "none rejected", requiredMode: wakeInjectModeNone, wantErr: true},
		{name: "inject-via rejected", requiredMode: wakeTargetInjectVia, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireWakeLockUsable(inspection, tc.requiredMode, nil)
			if tc.wantErr && err == nil {
				t.Fatalf("expected legacy wake to reject %q", tc.requiredMode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected legacy wake to accept %q: %v", tc.requiredMode, err)
			}
		})
	}
}

func TestRequireWakeLockUsableModeMatrix(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "matrix-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "fixed"})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	modes := []string{"", wakeInjectModeNone, wakeInjectModeRaw, wakeInjectModePaste, wakeTargetInjectVia}
	compatible := func(existing, requested string) bool {
		if existing == "" {
			return requested == wakeInjectModeRaw || requested == wakeInjectModePaste
		}
		return existing == requested
	}

	for _, existing := range modes {
		for _, requested := range modes[1:] {
			name := fmt.Sprintf("existing=%s/requested=%s", emptyAsLegacy(existing), requested)
			t.Run(name, func(t *testing.T) {
				lock := wakeLock{
					Root:     canonicalWakeRoot(root),
					Agent:    "codex",
					WakeMode: existing,
					TTY:      "test-tty",
				}
				if existing == wakeTargetInjectVia {
					lock = bindWakeLockToTarget(lock, target)
				}
				inspection := wakeLockInspection{
					Exists:            true,
					Status:            wakeLockValid,
					IdentityConfirmed: true,
					Root:              canonicalWakeRoot(root),
					Agent:             "codex",
					Lock:              lock,
				}
				var requestedTarget *wakeTarget
				if requested == wakeTargetInjectVia {
					requestedTarget = &target
				}
				err := requireWakeLockUsable(inspection, requested, requestedTarget)
				if compatible(existing, requested) && err != nil {
					t.Fatalf("compatible mode pair rejected: %v", err)
				}
				if !compatible(existing, requested) && err == nil {
					t.Fatal("incompatible mode pair accepted")
				}
			})
		}
	}
}

func emptyAsLegacy(mode string) string {
	if mode == "" {
		return "legacy-empty"
	}
	return mode
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsMissingTTY(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeTargetInjectVia,
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected unusable wake lock error")
	}
	if !strings.Contains(err.Error(), "not usable for --require-wake") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("existing lock should remain, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsBlankOrUnknownTTY(t *testing.T) {
	for _, tc := range []struct {
		name string
		tty  string
	}{
		{name: "blank", tty: ""},
		{name: "unknown", tty: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const wakePID = 4242
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == wakePID {
					return wakeProcessInfo{
						PID:        pid,
						Running:    true,
						StartToken: "start-1",
						BootID:     "boot-1",
						Executable: "/opt/homebrew/bin/amq",
						Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
					}
				}
				return wakeProcessInfo{PID: pid}
			})
			root := secureTempDirForTest(t)
			lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
				PID:          wakePID,
				TTY:          tc.tty,
				ProcessStart: "start-1",
				BootID:       "boot-1",
				Executable:   "/opt/homebrew/bin/amq",
				WakeMode:     wakeTargetInjectVia,
			})

			readyPath := filepath.Join(t.TempDir(), "wake.ready")
			injector := writeExecutableForTest(t, "injector")
			err := runWakeWithLoop([]string{
				"--root", root,
				"--me", "orchestrator",
				"--inject-via", injector,
				"--ready-file", readyPath,
				"--accept-existing-wake",
			}, func(cfg wakeConfig) error {
				t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
				return nil
			})
			if err == nil {
				t.Fatal("expected unusable wake lock error")
			}
			if !strings.Contains(err.Error(), "not usable for --require-wake") {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
				t.Fatalf("ready file should not exist, statErr=%v", statErr)
			}
			if _, statErr := os.Stat(lockPath); statErr != nil {
				t.Fatalf("existing lock should remain, statErr=%v", statErr)
			}
		})
	}
}

func TestRunWakeWithLoopAcceptExistingWakeAcceptsInjectViaUnknownTTY(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", injector},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "generation-1",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	writeWakePreparedForTest(t, root, "orchestrator")

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--inject-arg", "exec",
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an existing live wake lock: %#v", cfg)
		return nil
	})
	if err != nil {
		t.Fatalf("expected inject-via wake to satisfy ready file despite unknown tty, got %v", err)
	}
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("ready file should exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsDifferentInjector(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	existingInjector := writeExecutableForTest(t, "existing-injector")
	requestedInjector := writeExecutableForTest(t, "requested-injector")
	existingTarget := mustNewWakeTargetForTest(t, root, "orchestrator", existingInjector, []string{"exec", "fixed"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", existingInjector},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	}, existingTarget))
	if err := writeWakeTarget(root, "orchestrator", existingTarget); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", requestedInjector,
		"--inject-arg", "exec",
		"--inject-arg", "fixed",
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with a different existing injector: %#v", cfg)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "injector") {
		t.Fatalf("different injector error = %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsSameTTYDifferentSession(t *testing.T) {
	const wakePID = 4242
	ttyPath := filepath.Join(t.TempDir(), "amq-test-tty")
	if err := os.WriteFile(ttyPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write fake tty path: %v", err)
	}
	stubWakeCurrentTTY(t, func() string { return ttyPath })
	sidCalls := 0
	stubWakeProcessSID(t, func(pid int) (int, error) {
		sidCalls++
		if pid == wakePID {
			return 100, nil
		}
		return 200, nil
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          ttyPath,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeTargetInjectVia,
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an unusable existing wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected unusable wake lock error")
	}
	if !strings.Contains(err.Error(), "not usable for --require-wake") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("existing lock should remain, statErr=%v", statErr)
	}
	if sidCalls < 2 {
		t.Fatalf("expected same-TTY branch to inspect wake and current SIDs, got %d calls", sidCalls)
	}
}

func TestRunWakeWithLoopAcceptExistingWakeRejectsUnverifiedWake(t *testing.T) {
	const wakePID = 4242
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "test-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})

	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	injector := writeExecutableForTest(t, "injector")
	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", injector,
		"--ready-file", readyPath,
		"--accept-existing-wake",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with an unverified wake lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected unverified wake lock error")
	}
	if !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file should not exist, statErr=%v", statErr)
	}
}

func TestAcquireWakeLockSelfHealsPIDReusedByNonAMQ(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "new-start",
				BootID:     "boot-1",
				Executable: "/bin/sleep",
				Args:       []string{"/bin/sleep", "100"},
			}
		}
		if pid == os.Getpid() {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "self-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock should replace stale PID-reuse lock: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read replacement lock: %v", err)
	}
	var got wakeLock
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal replacement lock: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("replacement pid = %d, want %d", got.PID, os.Getpid())
	}
	if got.ProcessStart != "self-start" {
		t.Fatalf("replacement process_start = %q, want self-start", got.ProcessStart)
	}
}

func TestAcquireWakeLockTreatsUnknownBootIdentityAsUnverified(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lock       wakeLock
		process    wakeProcessInfo
		wantReason string
	}{
		{
			name: "boot id mismatch",
			lock: wakeLock{
				PID:          4242,
				ProcessStart: "start-1",
				BootID:       "recorded-boot",
				Executable:   "/opt/homebrew/bin/amq",
			},
			process: wakeProcessInfo{
				Running:    true,
				StartToken: "start-1",
				BootID:     "actual-boot",
				Executable: "/opt/homebrew/bin/amq",
			},
			wantReason: "boot id mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const reusedPID = 4242
			root := secureTempDirForTest(t)
			lockPath := writeWakeLockForTest(t, root, "orchestrator", tc.lock)
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == reusedPID {
					proc := tc.process
					proc.PID = pid
					proc.Args = []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root}
					return proc
				}
				return wakeProcessInfo{PID: pid}
			})

			cleanup, err := acquireWakeLock(root, "orchestrator", nil)
			if cleanup != nil {
				defer cleanup()
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantReason) || !strings.Contains(err.Error(), "unverified") {
				t.Fatalf("expected identity-mismatch unverified refusal, got %v", err)
			}
			if _, statErr := os.Stat(lockPath); statErr != nil {
				t.Fatalf("identity-mismatch lock should remain, stat=%v", statErr)
			}
		})
	}
}

func TestAcquireWakeLockReplacesProvenStartMismatchWhenBootMatches(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:          pid,
				Running:      true,
				StartToken:   "new-start",
				BootID:       "boot-1",
				InspectError: errors.New("executable unavailable"),
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read replacement lock: %v", err)
	}
	var got wakeLock
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal replacement lock: %v", err)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("replacement pid = %d, want %d", got.PID, os.Getpid())
	}
}

func TestAcquireWakeLockPreservesStartMismatchWhenBootIsUnknown(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		BootID:       "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:          pid,
				Running:      true,
				StartToken:   "new-start",
				BootID:       "100.000000000",
				Executable:   "/opt/homebrew/bin/amq",
				Args:         []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
				InspectError: errors.New("boot representations are not comparable"),
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "boot id mismatch") || !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("expected unknown-boot refusal, got %v", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain when boot identity is unknown, stat=%v", statErr)
	}
}

func TestAcquireWakeLockPreservesStartMismatchWithoutBootIdentity(t *testing.T) {
	const reusedPID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          reusedPID,
		ProcessStart: "old-start",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == reusedPID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "new-start",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "process start time mismatch") || !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("expected missing-boot refusal, got %v", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain without comparable boot identity, stat=%v", statErr)
	}
}

func TestAcquireWakeLockStartReadFailureIsUnverifiedNotMismatch(t *testing.T) {
	const pid = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          pid,
		ProcessStart: "old-start",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == pid {
			return wakeProcessInfo{
				PID:          gotPID,
				Running:      true,
				Executable:   "/opt/homebrew/bin/amq",
				InspectError: errors.New("permission denied"),
			}
		}
		return wakeProcessInfo{PID: gotPID}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("expected unverified lock error")
	}
	if !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("expected unverified error, got %v", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("unverified lock should remain, stat=%v", statErr)
	}
}

func TestAcquireWakeLockLegacyLiveLockDoesNotAutoDelete(t *testing.T) {
	const pid = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:        pid,
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == pid {
			return wakeProcessInfo{
				PID:        gotPID,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
			}
		}
		return wakeProcessInfo{PID: gotPID}
	})

	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("expected legacy unverified lock error")
	}
	if !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("expected unverified error, got %v", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("legacy unverified lock should remain, stat=%v", statErr)
	}
}

func TestRemoveWakeLockIfUnchangedRefusesChangedLock(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{PID: 4242})
	inspection := inspectWakeLock(root, "orchestrator")
	if !inspection.Exists {
		t.Fatal("expected lock inspection")
	}
	changed := wakeLock{
		PID:     4243,
		Root:    canonicalWakeRoot(root),
		Agent:   "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(changed)
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write changed lock: %v", err)
	}

	err := removeWakeLockIfUnchanged(inspection)
	if err == nil {
		t.Fatal("expected changed lock removal error")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("changed lock should remain, stat=%v", statErr)
	}
}

func TestRemoveWakeLockDoesNotDeleteReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{PID: 4242, Generation: "old"})
	inspection := inspectWakeLock(root, "orchestrator")
	if !inspection.Exists {
		t.Fatal("expected lock inspection")
	}

	replacement := wakeLock{
		PID:        4242,
		Root:       canonicalWakeRoot(root),
		Agent:      "orchestrator",
		Started:    time.Now().UTC().Format(time.RFC3339),
		Generation: "old",
	}
	data, _ := json.Marshal(replacement)
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove original lock: %v", err)
	}
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write byte-compatible replacement lock: %v", err)
	}

	err := removeWakeLockIfUnchanged(inspection)
	if err == nil {
		t.Fatal("expected replacement generation removal refusal")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("replacement lock should remain, stat=%v", statErr)
	}
}

func TestWakeCleanupDoesNotDeleteReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read acquired lock: %v", err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove acquired lock: %v", err)
	}
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write byte-compatible replacement: %v", err)
	}

	cleanup()
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("replacement lock should survive old cleanup: %v", statErr)
	}
}

func TestAcquireAndCleanupWaitForWakeLifecycleGuard(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWakeLifecycleGuard(root, "orchestrator", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	type acquireResult struct {
		cleanup func()
		err     error
	}
	acquired := make(chan acquireResult, 1)
	go func() {
		cleanup, err := acquireWakeLock(root, "orchestrator", nil)
		acquired <- acquireResult{cleanup: cleanup, err: err}
	}()
	time.Sleep(25 * time.Millisecond)
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("acquire mutated lock before lifecycle guard release: %v", err)
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("guard holder: %v", err)
	}
	result := <-acquired
	if result.err != nil {
		t.Fatalf("acquireWakeLock: %v", result.err)
	}

	entered = make(chan struct{})
	release = make(chan struct{})
	holderDone = make(chan error, 1)
	go func() {
		holderDone <- withWakeLifecycleGuard(root, "orchestrator", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	cleanupDone := make(chan struct{})
	go func() {
		result.cleanup()
		close(cleanupDone)
	}()
	time.Sleep(25 * time.Millisecond)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("cleanup removed lock before lifecycle guard release: %v", err)
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("guard holder: %v", err)
	}
	<-cleanupDone
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove exact lock: %v", err)
	}
}

func TestInspectWakeLockRejectsSymlinkAndFIFO(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, path string)
		wantError string
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "lock.json")
				if err := os.WriteFile(target, []byte(`{"pid":4242}`), 0o600); err != nil {
					t.Fatalf("write target lock: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink lock: %v", err)
				}
			},
			wantError: "must not be a symlink",
		},
		{
			name: "fifo",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("mkfifo lock: %v", err)
				}
			},
			wantError: "must be a regular file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentBase := fsq.AgentBase(root, "orchestrator")
			if err := os.MkdirAll(agentBase, 0o700); err != nil {
				t.Fatalf("mkdir agent base: %v", err)
			}
			tc.setup(t, filepath.Join(agentBase, ".wake.lock"))

			done := make(chan wakeLockInspection, 1)
			go func() {
				done <- inspectWakeLock(root, "orchestrator")
			}()

			select {
			case inspection := <-done:
				if !inspection.Exists || inspection.Status != wakeLockUnverified ||
					!strings.Contains(inspection.Reason, tc.wantError) {
					t.Fatalf("unexpected inspection: %#v", inspection)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("inspectWakeLock blocked")
			}
		})
	}
}

func TestShouldReplaceOrphanedWakeLockSignalsOnlyAfterRevalidation(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeTargetInjectVia,
	})
	killed := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			if killed {
				return wakeProcessInfo{PID: pid, Running: false}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	signals := []os.Signal{}
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		if pid != wakePID {
			t.Fatalf("signal pid = %d, want %d", pid, wakePID)
		}
		signals = append(signals, sig)
		if sig == os.Kill {
			killed = true
		}
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "no cooperative control endpoint") {
		t.Fatalf("expected missing-control refusal, got %v", err)
	}
	if replaced {
		t.Fatal("legacy inject-via orphan must not be replaced by PID")
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %d, want none", len(signals))
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock should remain after refusal, stat=%v", err)
	}
}

func TestShouldReplaceOrphanedWakeLockKeepsLockWhenKillDoesNotTerminate(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "start-1",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "live raw wake orphan") {
		t.Fatalf("expected live raw orphan refusal, got %v", err)
	}
	if replaced {
		t.Fatal("should not replace lock when old wake remains alive")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain after failed kill, stat=%v", statErr)
	}
}

func TestTerminateWakeProcessPreservesLiveWakeOnUnknownBootAfterSignal(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4343
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		info := wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
		if calls >= 3 { // after SIGTERM: still live, but boot identity is unavailable
			return info
		}
		info.BootID = "boot-1"
		return info
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error { signals = append(signals, sig); return nil })
	inspection := inspectWakeLock(root, "codex")
	if err := terminateWakeProcess(inspection); err == nil {
		t.Fatal("terminateWakeProcess unexpectedly declared success for live wake with unknown boot")
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want only SIGTERM", signals)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestTerminatePreservesLockOnUnknownInspectionAfterSIGTERM(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4350
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		if calls >= 3 {
			return wakeProcessInfo{PID: pid, Running: true, InspectError: errors.New("sysctl kinfo failed")}
		}
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		signals = append(signals, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "codex")
	err := terminateWakeProcess(inspection)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("terminateWakeProcess error = %v, want unknown-inspection error", err)
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want only SIGTERM", signals)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestTerminatePreservesLockOnUnknownInspectionAfterSIGKILL(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4351
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		if calls >= 5 {
			return wakeProcessInfo{PID: pid, Running: true, InspectError: errors.New("sysctl kinfo failed")}
		}
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		signals = append(signals, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "codex")
	err := terminateWakeProcess(inspection)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("terminateWakeProcess error = %v, want unknown-inspection error", err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want SIGTERM then SIGKILL", signals)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestTerminateWakeProcessPreservesLiveWakeOnShiftedBootClock(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4344
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "100.000000000", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		boot := "100.000000000"
		if calls >= 3 {
			boot = "200.000000000"
		}
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: boot, Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error { signals = append(signals, sig); return nil })
	inspection := inspectWakeLock(root, "codex")
	if err := terminateWakeProcess(inspection); err == nil {
		t.Fatal("terminateWakeProcess unexpectedly declared success after a shifted boot clock")
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want only SIGTERM", signals)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestTerminateWakeProcessPreservesLiveWakeOnShiftedLegacyBootInLegacyField(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4345
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: wakePID, TTY: "tty", ProcessStart: "start-1", BootID: "100.000000000", Executable: "/opt/homebrew/bin/amq"})
	calls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		calls++
		info := wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"}}
		if calls >= 3 {
			info.BootID = "9C0682F4-901B-4243-8B5C-287FAFB9AD0E"
			info.LegacyBootID = "200.000000000"
			return info
		}
		info.BootID = "100.000000000"
		return info
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error { return nil })
	inspection := inspectWakeLock(root, "codex")
	if err := terminateWakeProcess(inspection); err == nil {
		t.Fatal("terminateWakeProcess unexpectedly declared success after shifted legacy boot clock")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was removed or became unreadable: %v", err)
	}
}

func TestShouldReplaceOrphanedWakeLockRevalidatesBeforeSignal(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	})
	inspectCalls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			inspectCalls++
			if inspectCalls <= 2 {
				return wakeProcessInfo{
					PID:        pid,
					Running:    true,
					StartToken: "start-1",
					BootID:     "boot-1",
					Executable: "/opt/homebrew/bin/amq",
					Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root},
				}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "reused-start",
				BootID:     "boot-1",
				Executable: "/bin/sleep",
				Args:       []string{"/bin/sleep", "100"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		t.Fatalf("must not signal after process identity changes, got pid=%d sig=%v", pid, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil {
		t.Fatal("expected identity-changed error")
	}
	if replaced {
		t.Fatal("should not replace lock when process identity changes before signal")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain after aborted signal, stat=%v", statErr)
	}
}

func TestShouldReplaceOrphanedWakeLockRefusesLegacyOwnerStateWhenOwnerGone(t *testing.T) {
	requireBarePIDWakeTermination(t)
	const wakePID = 4242
	const ownerPID = 7777
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	owner := wakeOwner{PID: ownerPID, ProcessStart: "owner-start", BootID: "boot-1"}
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	target.Owner = &owner
	lockPath := writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	killed := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case wakePID:
			if killed {
				return wakeProcessInfo{PID: pid, Running: false}
			}
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "wake-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root, "--inject-via", injector},
			}
		case ownerPID:
			return wakeProcessInfo{PID: pid, Running: false}
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		if pid != wakePID {
			t.Fatalf("signal pid = %d, want %d", pid, wakePID)
		}
		if sig == os.Kill {
			killed = true
		}
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "recover-owner") {
		t.Fatalf("expected owner-state refusal, got %v", err)
	}
	if replaced || killed {
		t.Fatal("legacy inject-via wake must not be terminated by PID")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock should remain after refusal, stat=%v", err)
	}
}

func TestShouldReplaceOrphanedWakeLockRefusesLegacyOwnerStateWhenOwnerMatches(t *testing.T) {
	const wakePID = 4242
	const ownerPID = 7777
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	owner := wakeOwner{PID: ownerPID, ProcessStart: "owner-start", BootID: "boot-1"}
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	target.Owner = &owner
	lockPath := writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case wakePID:
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "wake-start",
				BootID:     "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--root", root, "--inject-via", injector},
			}
		case ownerPID:
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "owner-start",
				BootID:     "boot-1",
			}
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		t.Fatalf("must not signal owner-matched inject-via wake, got pid=%d sig=%v", pid, sig)
		return nil
	})

	inspection := inspectWakeLock(root, "orchestrator")
	replaced, err := shouldReplaceOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "recover-owner") {
		t.Fatalf("expected owner-state refusal, got %v", err)
	}
	if replaced {
		t.Fatal("legacy owner-bearing wake should not be replaced")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("lock should remain for owner-matched wake, stat=%v", statErr)
	}
}

func TestRunWakeWithLoopRejectsRecentlyCorruptLock(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	if err := os.WriteFile(lockPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt lock: %v", err)
	}

	err := runWakeWithLoop([]string{
		"--root", root,
		"--me", "orchestrator",
		"--inject-via", writeExecutableForTest(t, "injector"),
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with recent corrupt lock: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected recent corrupt lock error")
	}
	if !strings.Contains(err.Error(), "being created") {
		t.Fatalf("expected being-created error, got %v", err)
	}
}

func TestWaitForWakeReadyReturnsWhenReadyFileAppears(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-1",
	})
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	if err := waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second); err != nil {
		t.Fatalf("waitForWakeReady: %v", err)
	}
}

func TestWaitForWakeReadyFailsWhenWakeExitsBeforeReady(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	err := waitForWakeReady(cmd.Process, readyPath, t.TempDir(), "orchestrator", time.Second)
	if err == nil {
		t.Fatal("expected readiness failure")
	}
	if !strings.Contains(err.Error(), "amq wake exited before becoming ready") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForWakeReadyAcceptsReadyFileWrittenBeforeExit(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-1",
	})
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	if err := waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second); err != nil {
		t.Fatalf("waitForWakeReady should accept ready file written before exit: %v", err)
	}
}

func TestWaitForWakeReadyRefusesLegacyReadyFile(t *testing.T) {
	root := secureTempDirForTest(t)
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatalf("write legacy ready file: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err := waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("expected legacy readiness refusal, got %v", err)
	}
}

func TestWaitForWakeReadyRefusesEmptyReadyFile(t *testing.T) {
	root := secureTempDirForTest(t)
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := os.WriteFile(readyPath, nil, 0o600); err != nil {
		t.Fatalf("write empty ready file: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err := waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("expected empty readiness refusal, got %v", err)
	}
}

func TestAcceptExistingReadinessNotPublishedOnReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: 4242, Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-1",
	})
	expected := inspectWakeLock(root, "orchestrator")
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove original lock: %v", err)
	}
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: 4242, Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-2",
	})
	readyPath := filepath.Join(t.TempDir(), "wake.ready")

	err := writeWakeReadyFile(root, "orchestrator", readyPath, expected)
	if err == nil {
		t.Fatal("expected replacement readiness refusal")
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("replacement readiness must not be published, statErr=%v", statErr)
	}
}

func TestCleanupTerminatedWakeLockRemovesOnlyCapturedStaleGeneration(t *testing.T) {
	const wakePID = 4242
	running := true
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    running,
			StartToken: "start-1",
			BootID:     "boot-1",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
		}
	})

	t.Run("captured generation", func(t *testing.T) {
		root := secureTempDirForTest(t)
		lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
			PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
		})
		expected := inspectWakeLock(root, "orchestrator")
		running = false
		if err := cleanupTerminatedWakeLock(expected); err != nil {
			t.Fatalf("cleanupTerminatedWakeLock: %v", err)
		}
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("captured stale lock was not removed, statErr=%v", err)
		}
	})

	t.Run("replacement generation", func(t *testing.T) {
		running = true
		root := secureTempDirForTest(t)
		lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
			PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
		})
		expected := inspectWakeLock(root, "orchestrator")
		if err := os.Remove(lockPath); err != nil {
			t.Fatalf("remove captured lock: %v", err)
		}
		writeWakeLockForTest(t, root, "orchestrator", wakeLock{
			PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq", Generation: "generation-2",
		})
		running = false
		if err := cleanupTerminatedWakeLock(expected); err != nil {
			t.Fatalf("cleanup replacement: %v", err)
		}
		if _, err := os.Stat(lockPath); err != nil {
			t.Fatalf("replacement generation was removed: %v", err)
		}
	})
}

func TestTerminateWakeHelperProcessKillsWaitsAndRemovesCapturedLock(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	wakePID := cmd.Process.Pid
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		running := syscall.Kill(pid, 0) == nil
		return wakeProcessInfo{
			PID: pid, Running: running, StartToken: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
		}
	})
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: wakePID, ProcessStart: "start-1", BootID: "boot-1",
		Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
	})
	waiter := newWakeProcessWaiter(cmd.Process)
	if err := terminateWakeHelperProcess(cmd.Process, waiter, root, "orchestrator"); err != nil {
		t.Fatalf("terminateWakeHelperProcess: %v", err)
	}
	if waiter.state == nil {
		t.Fatalf("wake helper was not waited: state=%v", waiter.state)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("terminated helper lock was not removed, statErr=%v", err)
	}
}

func TestTerminateWakeHelperProcessRemovesLockCommittedAfterFirstInspection(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	root := secureTempDirForTest(t)
	lockPath := filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock")
	originalKill := killWakeHelperProcess
	killWakeHelperProcess = func(proc *os.Process) error {
		writeWakeLockForTest(t, root, "orchestrator", wakeLock{
			PID: proc.Pid, Generation: "generation-after-first-inspection",
		})
		return originalKill(proc)
	}
	t.Cleanup(func() { killWakeHelperProcess = originalKill })

	waiter := newWakeProcessWaiter(cmd.Process)
	if err := terminateWakeHelperProcess(cmd.Process, waiter, root, "orchestrator"); err != nil {
		t.Fatalf("terminateWakeHelperProcess: %v", err)
	}
	if waiter.state == nil {
		t.Fatal("wake helper was not waited")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("post-inspection child lock was not removed, statErr=%v", err)
	}
}

func TestWaitForWakeReadyRefusesLockReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	lockPath := inspection.LockPath
	replacement := inspection.Lock
	replacement.Generation = "replacement-generation"
	data, _ := json.Marshal(replacement)
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove original lock: %v", err)
	}
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write replacement lock: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err = waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("expected replacement generation refusal, got %v", err)
	}
}

func TestWaitForWakeReadyRefusesTargetReplacement(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	cleanup, err := acquireWakeLock(root, "orchestrator", &target)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	tampered := target
	tampered.InjectArgs = []string{"different"}
	if err := writeWakeTarget(root, "orchestrator", tampered); err != nil {
		t.Fatalf("write replacement target: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err = waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "does not match wake lock") {
		t.Fatalf("expected replacement target refusal, got %v", err)
	}
}

func TestWaitForWakeReadyRefusesStrayTargetForTargetlessLock(t *testing.T) {
	root := secureTempDirForTest(t)
	cleanup, err := acquireWakeLock(root, "orchestrator", nil)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	defer cleanup()
	inspection := inspectWakeLock(root, "orchestrator")
	readyPath := filepath.Join(t.TempDir(), "wake.ready")
	if err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection); err != nil {
		t.Fatalf("writeWakeReadyFile: %v", err)
	}
	injector := writeExecutableForTest(t, "injector")
	strayTarget := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	if err := writeWakeTarget(root, "orchestrator", strayTarget); err != nil {
		t.Fatalf("write stray target: %v", err)
	}
	cmd := exec.Command("sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err = waitForWakeReady(cmd.Process, readyPath, root, "orchestrator", time.Second)
	if err == nil || !strings.Contains(err.Error(), "target does not match current wake lock") {
		t.Fatalf("expected stray target refusal for targetless lock, got %v", err)
	}
}

func TestWriteWakeReadyFileRejectsSymlink(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "orchestrator", wakeLock{
		PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "orchestrator",
		Started: time.Now().UTC().Format(time.RFC3339), Generation: "generation-1",
	})
	inspection := inspectWakeLock(root, "orchestrator")
	dir := t.TempDir()
	target := filepath.Join(dir, "target.ready")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	readyPath := filepath.Join(dir, "wake.ready")
	if err := os.Symlink(target, readyPath); err != nil {
		t.Fatalf("symlink ready file: %v", err)
	}

	err := writeWakeReadyFile(root, "orchestrator", readyPath, inspection)
	if err == nil {
		t.Fatal("expected wake ready symlink rejection")
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "old\n" {
		t.Fatalf("symlink target changed: data=%q err=%v", got, readErr)
	}
}

func TestConfigureRepairWakeCommandDetachesOutput(t *testing.T) {
	output, err := os.OpenFile(filepath.Join(t.TempDir(), "repair.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer func() { _ = output.Close() }()

	cmd := exec.Command("amq")
	configureRepairWakeCommand(cmd, output)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("repair wake command should start in a new session: %#v", cmd.SysProcAttr)
	}
	if cmd.Stdout != output || cmd.Stderr != output {
		t.Fatalf("repair wake command should redirect stdout/stderr to repair log")
	}
	if cmd.Stdout == os.Stdout || cmd.Stderr == os.Stderr {
		t.Fatalf("repair wake command must not inherit parent stdout/stderr")
	}
}

func TestOpenWakeRepairOutputCreatesPrivateLog(t *testing.T) {
	root := secureTempDirForTest(t)
	output, err := openWakeRepairOutputForTest(root, "orchestrator")
	if err != nil {
		t.Fatalf("openWakeRepairOutput: %v", err)
	}
	path := output.Name()
	if err := output.Close(); err != nil {
		t.Fatalf("close output: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat repair output: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("repair output mode = %o, want 0600", got)
	}
}

func TestOpenWakeRepairOutputRejectsSymlinkLog(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	target := filepath.Join(t.TempDir(), "repair.log")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write target log: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(agentBase, ".wake.repair.log")); err != nil {
		t.Fatalf("symlink repair log: %v", err)
	}

	output, err := openWakeRepairOutputForTest(root, "orchestrator")
	if err == nil {
		_ = output.Close()
		t.Fatal("expected symlink repair log rejection")
	}
	if !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestOpenWakeRepairOutputRejectsFIFOWithoutBlocking(t *testing.T) {
	root := secureTempDirForTest(t)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		t.Fatalf("mkdir agent base: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(agentBase, ".wake.repair.log"), 0o600); err != nil {
		t.Fatalf("mkfifo repair log: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		output, err := openWakeRepairOutputForTest(root, "orchestrator")
		if output != nil {
			_ = output.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
			t.Fatalf("expected FIFO rejection, got %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("openWakeRepairOutput blocked on FIFO")
	}
}

func openWakeRepairOutputForTest(root, me string) (*os.File, error) {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, err
	}
	defer func() { _ = agentDir.Close() }()
	return openWakeRepairOutputInDir(agentDir)
}

func TestRunWakeRepairJSONRejectsFIFOLogWithoutBlocking(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	writeWakeRepairFloorForTest(t, root, "orchestrator", target, nil)
	agentBase := fsq.AgentBase(root, "orchestrator")
	if err := syscall.Mkfifo(filepath.Join(agentBase, ".wake.repair.log"), 0o600); err != nil {
		t.Fatalf("mkfifo repair log: %v", err)
	}

	stdout, _, runErr := captureWakeRepairOutput(t, func() error {
		return runWakeRepair([]string{"--root", root, "--me", "orchestrator", "--json"})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "regular file") {
		t.Fatalf("runWakeRepair error = %v, want regular-file refusal", runErr)
	}
	var result wakeRepairResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal output: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "error" || !strings.Contains(result.Reason, "regular file") {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRunWakeWithLoopRejectsInjectArgWithoutInjectVia(t *testing.T) {
	err := runWakeWithLoop([]string{
		"--root", t.TempDir(),
		"--me", "orchestrator",
		"--inject-arg", "exec",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with invalid flags: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "--inject-arg requires --inject-via") {
		t.Fatalf("expected inject-arg usage error, got %v", err)
	}
}

func TestRunWakeWithLoopRejectsInvalidRepairLineageFlags(t *testing.T) {
	injector := writeExecutableForTest(t, "injector")
	tests := []struct {
		name     string
		args     []string
		ownerEnv string
		want     string
	}{
		{
			name: "blank lineage",
			args: []string{"--repair-lineage= "},
			want: "--repair-lineage must not be blank",
		},
		{
			name: "requires inject via",
			args: []string{"--repair-lineage", "dead-generation"},
			want: "--repair-lineage requires --inject-via",
		},
		{
			name: "conflicts with baseline existing",
			args: []string{
				"--repair-lineage", "dead-generation",
				"--inject-via", injector,
				"--baseline-existing",
			},
			want: "--repair-lineage cannot be combined with --baseline-existing",
		},
		{
			name: "requires private handoff before owner inspection",
			args: []string{
				"--repair-lineage", "dead-generation",
				"--inject-via", injector,
			},
			ownerEnv: `{"pid":4242}`,
			want:     "wake repair requires a private source/admission handoff",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envWakeOwner, tc.ownerEnv)
			args := []string{"--root", secureTempDirForTest(t), "--me", "orchestrator"}
			args = append(args, tc.args...)
			err := runWakeWithLoop(args, func(cfg wakeConfig) error {
				t.Fatalf("loop should not run with invalid repair lineage: %#v", cfg)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunWakeWithLoopRejectsNoneWithInputTransports(t *testing.T) {
	injector := writeExecutableForTest(t, "injector")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "inject via", args: []string{"--inject-via", injector}, want: "--inject-via"},
		{name: "inject arg", args: []string{"--inject-arg", "exec"}, want: "--inject-arg"},
		{name: "inject cmd", args: []string{"--inject-cmd", "amq drain"}, want: "--inject-cmd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--root", t.TempDir(), "--me", "orchestrator", "--inject-mode", "none"}
			args = append(args, tt.args...)
			err := runWakeWithLoop(args, func(cfg wakeConfig) error {
				t.Fatalf("loop should not run with invalid flags: %#v", cfg)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "none") {
				t.Fatalf("error = %v, want none-mode conflict mentioning %s", err, tt.want)
			}
		})
	}
}

func TestRunWakeWithLoopRejectsNonPositiveInjectTimeout(t *testing.T) {
	err := runWakeWithLoop([]string{
		"--root", t.TempDir(),
		"--me", "orchestrator",
		"--inject-via", writeExecutableForTest(t, "injector"),
		"--inject-timeout", "0",
	}, func(cfg wakeConfig) error {
		t.Fatalf("loop should not run with invalid timeout: %#v", cfg)
		return nil
	})
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "--inject-timeout must be > 0") {
		t.Fatalf("expected inject-timeout usage error, got %v", err)
	}
}

func TestWakeHealthCheckSkipsTTYForInjectVia(t *testing.T) {
	err := wakeHealthCheck(wakeConfig{injectVia: "/tmp/injector"}, func() bool {
		return false
	})
	if err != nil {
		t.Fatalf("expected external injection health check to skip TTY, got %v", err)
	}
}

func TestWakeHealthCheckSkipsTTYForNoneMode(t *testing.T) {
	err := wakeHealthCheck(wakeConfig{injectMode: wakeInjectModeNone}, func() bool {
		return false
	})
	if err != nil {
		t.Fatalf("expected none mode health check to skip TTY, got %v", err)
	}
}

func TestWakeHealthCheckExitsWhenInjectViaOwnerGone(t *testing.T) {
	owner := wakeOwner{PID: 4242, ProcessStart: "owner-start", BootID: "boot-1"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	err := wakeHealthCheck(wakeConfig{injectVia: "/tmp/injector", wakeOwner: &owner}, func() bool {
		return false
	})
	if err == nil {
		t.Fatal("expected owner liveness failure")
	}
	if !strings.Contains(err.Error(), "owner pid 4242 is not running") {
		t.Fatalf("unexpected owner liveness error: %v", err)
	}
}

func TestWakeHealthCheckExitsWhenInjectViaOwnerIdentityChanges(t *testing.T) {
	owner := wakeOwner{PID: 4242, ProcessStart: "owner-start", BootID: "boot-1", SessionID: 99}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "other-start",
			BootID:     "boot-1",
		}
	})

	err := wakeHealthCheck(wakeConfig{injectVia: "/tmp/injector", wakeOwner: &owner}, func() bool {
		return false
	})
	if err == nil {
		t.Fatal("expected owner identity failure")
	}
	if !strings.Contains(err.Error(), "owner process start changed") {
		t.Fatalf("unexpected owner identity error: %v", err)
	}
}

func TestWakeHealthCheckKeepsInjectViaWhenOwnerMatches(t *testing.T) {
	owner := wakeOwner{PID: 4242, ProcessStart: "owner-start", BootID: "boot-1"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "owner-start",
			BootID:     "boot-1",
		}
	})

	err := wakeHealthCheck(wakeConfig{injectVia: "/tmp/injector", wakeOwner: &owner}, func() bool {
		return false
	})
	if err != nil {
		t.Fatalf("expected owner-matched inject-via health check to pass, got %v", err)
	}
}

func TestWakeCommandEnvCarriesOwnerToken(t *testing.T) {
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	env, err := wakeCommandEnv([]string{
		"PATH=/bin",
		envRoot + "=/old/root",
		envWakeOwner + `={"pid":111}`,
	}, "/new/root", &owner)
	if err != nil {
		t.Fatalf("wakeCommandEnv: %v", err)
	}
	if got := testEnvValue(env, envRoot); got != "/new/root" {
		t.Fatalf("%s = %q, want /new/root", envRoot, got)
	}
	var decoded wakeOwner
	if err := json.Unmarshal([]byte(testEnvValue(env, envWakeOwner)), &decoded); err != nil {
		t.Fatalf("decode %s: %v", envWakeOwner, err)
	}
	if decoded != owner {
		t.Fatalf("decoded owner = %#v, want %#v", decoded, owner)
	}

	env, err = wakeCommandEnv(env, "/raw/root", nil)
	if err != nil {
		t.Fatalf("wakeCommandEnv without owner: %v", err)
	}
	if got := testEnvValue(env, envRoot); got != "/raw/root" {
		t.Fatalf("%s = %q, want /raw/root", envRoot, got)
	}
	if got := testEnvValue(env, envWakeOwner); got != "" {
		t.Fatalf("%s should be cleared without owner, got %q", envWakeOwner, got)
	}
}

func TestWakeHealthCheckRequiresTTYForTIOCSTI(t *testing.T) {
	err := wakeHealthCheck(wakeConfig{}, func() bool {
		return false
	})
	if err == nil {
		t.Fatal("expected TTY health failure")
	}
	if err.Error() != "TTY no longer available" {
		t.Fatalf("expected TTY health error, got %v", err)
	}
}

func testEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
