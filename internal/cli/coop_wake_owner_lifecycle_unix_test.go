//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	coopWakeSignalHelperEnv = "AMQ_TEST_COOP_WAKE_SIGNAL_HELPER"
	coopWakePTYHelperEnv    = "AMQ_TEST_COOP_WAKE_PTY_HELPER"
)

func TestCoopWakeArgsDisableInterruptInjection(t *testing.T) {
	for _, mode := range []string{
		wakeInjectModeAuto,
		wakeInjectModeRaw,
		wakeInjectModePaste,
		wakeInjectModeNone,
	} {
		t.Run(mode, func(t *testing.T) {
			args := buildCoopWakeArgs("codex", "/tmp/root", mode, "", nil, "/tmp/ready")
			found := false
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--interrupt-cmd" && args[i+1] == "none" {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("coop wake args permit Ctrl-C injection: %#v", args)
			}
		})
	}
}

func TestCoopWakeAllModesInstallExactOwnerSupervisor(t *testing.T) {
	owner := currentAuthoritativeOwnerForCoopWakeTest(t)
	encoded, err := encodeWakeOwnerEnv(owner)
	if err != nil {
		t.Fatalf("encode owner: %v", err)
	}

	oldAvailable := wakeTIOCSTIAvailable
	oldIsTTY := wakeInputIsTTY
	oldBindTerminalAuthority := bindWakeTerminalAuthorityForWake
	wakeTIOCSTIAvailable = func() bool { return true }
	wakeInputIsTTY = func() bool { return true }
	bindWakeTerminalAuthorityForWake = func(
		generation wakeLockInspection,
		stop <-chan struct{},
	) (*wakeTerminalAuthority, error) {
		if !generation.Exists || generation.Lock.Generation == "" {
			return nil, errors.New("test terminal authority received no exact generation")
		}
		return &wakeTerminalAuthority{
			generation:  generation,
			controlStop: stop,
		}, nil
	}
	t.Cleanup(func() {
		wakeTIOCSTIAvailable = oldAvailable
		wakeInputIsTTY = oldIsTTY
		bindWakeTerminalAuthorityForWake = oldBindTerminalAuthority
	})

	for _, mode := range []string{
		wakeInjectModeAuto,
		wakeInjectModeRaw,
		wakeInjectModePaste,
		wakeInjectModeNone,
	} {
		t.Run(mode, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			t.Setenv(envWakeOwner, encoded)
			t.Setenv(envWakePrivateStopFD, "")

			loopCalled := false
			err := runWakeWithLoop(
				[]string{
					"--root", root,
					"--me", "codex",
					"--inject-mode", mode,
					"--interrupt=false",
				},
				func(cfg wakeConfig) error {
					loopCalled = true
					if cfg.controlStop == nil {
						return errors.New("coop-owned wake reached its loop without an exact owner supervisor")
					}
					select {
					case <-cfg.controlStop:
						return errors.New("live exact owner supervisor was already stopped")
					default:
					}
					return nil
				},
			)
			if err != nil {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if !loopCalled {
				t.Fatalf("mode %s: wake loop was not reached", mode)
			}
		})
	}
}

func TestCoopWakeOwnerMetadataFailureNeverStartsOwnerlessWake(t *testing.T) {
	tests := []struct {
		name       string
		ownerEnv   string
		observeErr error
	}{
		{
			name:     "malformed owner metadata",
			ownerEnv: `{"pid":`,
		},
		{
			name:     "unsupported exact owner observer",
			ownerEnv: mustEncodedCurrentOwnerForCoopWakeTest(t),
			observeErr: &wakeOwnerChildCapabilityUnsupportedError{
				Err: syscall.ENOSYS,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			t.Setenv(envWakeOwner, test.ownerEnv)
			t.Setenv(envWakePrivateStopFD, "")

			if test.observeErr != nil {
				oldObserve := observeAuthoritativeWakeOwner
				observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
					return wakeOwnerObservation{
						State:                 wakeOwnerUnknown,
						Reason:                "test observer unavailable",
						CapabilityUnsupported: true,
					}, test.observeErr
				}
				t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
			}

			loopCalled := false
			childErr := runWakeWithLoop(
				[]string{
					"--root", root,
					"--me", "codex",
					"--inject-mode", wakeInjectModeNone,
					"--interrupt=false",
				},
				func(wakeConfig) error {
					loopCalled = true
					return errors.New("ownerless wake loop ran")
				},
			)
			if childErr == nil {
				t.Fatal("owner supervision failure started an ownerless wake")
			}
			if loopCalled {
				t.Fatal("owner supervision failure reached the wake loop")
			}
			if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
				t.Fatalf("owner supervision failure published a wake lock: %#v", inspection)
			}

			if err := handleCoopWakeSetupFailure(false, wakeLockInspection{}, "start coop-owned wake", childErr); err != nil {
				t.Fatalf("optional wake should continue with no wake, got %v", err)
			}
			if err := handleCoopWakeSetupFailure(true, wakeLockInspection{}, "start coop-owned wake", childErr); err == nil {
				t.Fatal("--require-wake accepted an unavailable exact owner observer")
			}
		})
	}
}

func TestCoopWakeNeverReusesPreparedOwnerlessWake(t *testing.T) {
	ownerEnv := mustEncodedCurrentOwnerForCoopWakeTest(t)

	oldAvailable := wakeTIOCSTIAvailable
	oldIsTTY := wakeInputIsTTY
	wakeTIOCSTIAvailable = func() bool { return true }
	wakeInputIsTTY = func() bool { return true }
	t.Cleanup(func() {
		wakeTIOCSTIAvailable = oldAvailable
		wakeInputIsTTY = oldIsTTY
	})

	for _, mode := range []string{
		wakeInjectModeRaw,
		wakeInjectModePaste,
		wakeInjectModeNone,
	} {
		t.Run(mode, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			oldTTY := getWakeCurrentTTY
			getWakeCurrentTTY = func() string { return "test-tty" }
			t.Cleanup(func() { getWakeCurrentTTY = oldTTY })
			realProcess := inspectWakeProcess(os.Getpid())
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid != os.Getpid() {
					return wakeProcessInfo{PID: pid}
				}
				return wakeProcessInfo{
					PID:        pid,
					Running:    true,
					StartToken: realProcess.StartToken,
					BootID:     realProcess.BootID,
					Executable: "amq",
					Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
				}
			})

			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				wakeMode: mode,
			})
			if err != nil {
				t.Fatalf("install unsupervised wake: %v", err)
			}
			defer cleanup()
			existing := inspectWakeLock(root, "codex")
			if !confirmedLiveWake(existing) {
				t.Fatalf("unsupervised fixture is not a prepared live wake: %#v", existing)
			}
			writeWakePreparedForTest(t, root, "codex")

			t.Setenv(envWakeOwner, ownerEnv)
			t.Setenv(envWakePrivateStopFD, "")
			readyPath := filepath.Join(t.TempDir(), "ready")
			loopCalled := false
			err = runWakeWithLoop(
				[]string{
					"--root", root,
					"--me", "codex",
					"--inject-mode", mode,
					"--ready-file", readyPath,
					"--accept-existing-wake",
					"--interrupt=false",
				},
				func(wakeConfig) error {
					loopCalled = true
					return nil
				},
			)
			if err == nil {
				t.Fatal("prepared ownerless wake satisfied a coop-owned launch")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "owner") {
				t.Fatalf("ownerless reuse refusal = %v, want exact owner reason", err)
			}
			if loopCalled {
				t.Fatal("second coop-owned wake unexpectedly acquired the existing generation")
			}
			if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
				t.Fatalf("ownerless generation published coop readiness: %v", statErr)
			}
			current := inspectWakeLock(root, "codex")
			if !sameWakeLockGeneration(existing, current) {
				t.Fatalf("rejected reuse changed the existing wake: before=%#v after=%#v", existing, current)
			}
		})
	}
}

func TestKernelOwnerObserverIgnoresDescendantInheritedFDs(t *testing.T) {
	for _, exitKind := range []string{"normal", "crash"} {
		t.Run(exitKind, func(t *testing.T) {
			dir := t.TempDir()
			descendantPath := filepath.Join(dir, "descendant.pid")
			exitGate := filepath.Join(dir, "owner-exit")

			readEnd, writeEnd, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			defer func() { _ = readEnd.Close() }()

			script := `
sleep 30 &
printf '%s\n' "$!" > "$1"
while [ ! -e "$2" ]; do sleep 0.01; done
if [ "$3" = crash ]; then
	kill -KILL $$
fi
exit 0
`
			cmd := exec.Command("/bin/sh", "-c", script, "sh", descendantPath, exitGate, exitKind)
			cmd.ExtraFiles = []*os.File{writeEnd}
			if err := cmd.Start(); err != nil {
				_ = writeEnd.Close()
				t.Fatalf("start owner: %v", err)
			}
			_ = writeEnd.Close()
			t.Cleanup(func() {
				if cmd.Process != nil && cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_, _ = cmd.Process.Wait()
				}
			})

			waitForCoopWakePathForTest(t, descendantPath, 3*time.Second)
			owner := authoritativeOwnerForPIDForCoopWakeTest(t, cmd.Process.Pid)
			observation, err := observeAuthoritativeWakeOwner(owner)
			if err != nil {
				t.Fatalf("observe exact owner: %v", err)
			}
			defer func() { _ = observation.Close() }()
			if observation.State != wakeOwnerSame {
				t.Fatalf("owner observation = %#v, want live exact owner", observation)
			}
			if observation.Done() == nil {
				t.Fatal("kernel owner observation has no death signal")
			}

			if err := os.WriteFile(exitGate, []byte("exit\n"), 0o600); err != nil {
				t.Fatalf("release owner: %v", err)
			}
			waitErr := cmd.Wait()
			if exitKind == "normal" && waitErr != nil {
				t.Fatalf("normal owner exit: %v", waitErr)
			}
			if exitKind == "crash" && waitErr == nil {
				t.Fatal("crashing owner exited successfully")
			}

			descendantPID := readPIDForCoopWakeTest(t, descendantPath)
			descendant, err := os.FindProcess(descendantPID)
			if err != nil {
				t.Fatalf("find descendant: %v", err)
			}
			t.Cleanup(func() {
				_ = descendant.Kill()
			})
			if err := descendant.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("FD-inheriting descendant is not alive: %v", err)
			}

			if err := readEnd.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
				t.Fatalf("set pipe deadline: %v", err)
			}
			var one [1]byte
			_, pipeErr := readEnd.Read(one[:])
			var netErr net.Error
			if !errors.As(pipeErr, &netErr) || !netErr.Timeout() {
				t.Fatalf("ordinary inherited pipe reported owner death while descendant lived: %v", pipeErr)
			}

			select {
			case <-observation.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("kernel-bound observation did not report exact owner exit")
			}
		})
	}
}

func TestKernelOwnerObserverRegistrationRejectsChangedIdentity(t *testing.T) {
	owner := currentAuthoritativeOwnerForCoopWakeTest(t)
	owner.ProcessStart = differentProcessStartForCoopWakeTest(owner.ProcessStart)

	observation, err := observeAuthoritativeWakeOwner(owner)
	if err != nil {
		t.Fatalf("observe changed identity: %v", err)
	}
	defer func() { _ = observation.Close() }()
	if observation.State != wakeOwnerDead {
		t.Fatalf("changed identity state = %s (%s), want dead exact owner", observation.State, observation.Reason)
	}
	select {
	case <-observation.Done():
	default:
		t.Fatal("registration identity change left readiness-capable owner observation open")
	}
}

func TestOwnerDeathGatesTerminalInjection(t *testing.T) {
	oldInject := tiocstiInject
	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	t.Cleanup(func() {
		tiocstiInject = oldInject
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}

	for _, mode := range []string{wakeInjectModeRaw, wakeInjectModePaste} {
		t.Run(mode+"/already-dead", func(t *testing.T) {
			stop := make(chan struct{})
			close(stop)
			calls := 0
			tiocstiInject = func(string) error {
				calls++
				return nil
			}
			err := injectNotification(&wakeConfig{
				me:          "codex",
				injectMode:  mode,
				controlStop: stop,
			}, "must not inject", false)
			if err != nil {
				t.Fatalf("closed owner gate: %v", err)
			}
			if calls != 0 {
				t.Fatalf("closed owner gate allowed %d terminal injection(s)", calls)
			}
		})

		t.Run(mode+"/dies-during-injection", func(t *testing.T) {
			stop := make(chan struct{})
			var calls []string
			tiocstiInject = func(text string) error {
				calls = append(calls, text)
				if len(calls) == 1 {
					close(stop)
				}
				return nil
			}
			err := injectNotification(&wakeConfig{
				me:          "codex",
				injectMode:  mode,
				controlStop: stop,
			}, "one in-flight write at most", false)
			if err != nil {
				t.Fatalf("mid-injection owner death: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("owner death allowed follow-up terminal injection: %#v", calls)
			}
		})
	}
}

func TestCoopRawDoorbellIsFixedASCIIAndShellInert(t *testing.T) {
	oldInject := tiocstiInject
	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	t.Cleanup(func() {
		tiocstiInject = oldInject
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}

	stop := make(chan struct{})
	var injected []string
	tiocstiInject = func(text string) error {
		injected = append(injected, text)
		return nil
	}
	dynamic := "AMQ [session/a]; sender=$(touch /tmp/pwned) subject='approve?' count=999 custom=\x1b[31m"
	err := injectNotification(&wakeConfig{
		me:          "codex",
		injectMode:  wakeInjectModeRaw,
		controlStop: stop,
	}, dynamic, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(injected) == 0 || injected[0] != coopWakeDoorbell {
		t.Fatalf("coop text injection = %#v, want fixed first chunk %q", injected, coopWakeDoorbell)
	}
	for _, chunk := range injected {
		for _, value := range []byte(chunk) {
			if value > 0x7f {
				t.Fatalf("coop injection contains non-ASCII byte %#x: %#v", value, injected)
			}
		}
	}
	if strings.Contains(strings.Join(injected, ""), "session/a") ||
		strings.Contains(strings.Join(injected, ""), "sender=") ||
		strings.Contains(strings.Join(injected, ""), "approve") ||
		strings.Contains(strings.Join(injected, ""), "999") ||
		strings.Contains(strings.Join(injected, ""), "\x1b") {
		t.Fatalf("coop injection leaked message-derived bytes: %#v", injected)
	}
}

func TestCoopUrgentDoorbellNeverInjectsCtrlCAndRefusalKeepsPending(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	msg := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       "urgent-owner-death",
			From:     "attacker-controlled-sender",
			To:       []string{"codex"},
			Thread:   "p2p/attacker__codex",
			Subject:  "dynamic subject must not enter terminal",
			Created:  "2026-07-24T00:00:00Z",
			Priority: "urgent",
			Labels:   []string{"interrupt"},
		},
		Body: "durable pending body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal urgent message: %v", err)
	}
	path, err := deliverToInboxForTest(t, root, "codex", "urgent-owner-death.md", data)
	if err != nil {
		t.Fatalf("deliver urgent message: %v", err)
	}

	stop := make(chan struct{})
	close(stop)
	oldInject := tiocstiInject
	var injected []string
	tiocstiInject = func(text string) error {
		injected = append(injected, text)
		return nil
	}
	t.Cleanup(func() { tiocstiInject = oldInject })
	cfg := &wakeConfig{
		me:                "codex",
		root:              root,
		session:           "dynamic-session",
		injectMode:        wakeInjectModeRaw,
		controlStop:       stop,
		previewLen:        48,
		interrupt:         true,
		interruptKey:      "",
		interruptLabel:    "interrupt",
		interruptPriority: "urgent",
	}
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify urgent pending item: %v", err)
	}
	if len(injected) != 0 {
		t.Fatalf("refused urgent doorbell injected terminal bytes: %#v", injected)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("refused doorbell consumed durable pending item: %v", err)
	}
}

func TestCoopRawDoorbellRechecksGenerationBeforeEveryWrite(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeRaw,
	})
	if err != nil {
		t.Fatalf("acquire wake: %v", err)
	}
	defer cleanup()
	original := inspectWakeLock(root, "codex")

	oldInject := tiocstiInject
	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	t.Cleanup(func() {
		tiocstiInject = oldInject
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}

	stop := make(chan struct{})
	var injected []string
	tiocstiInject = func(text string) error {
		injected = append(injected, text)
		if len(injected) == 1 {
			replacement := original.Lock
			replacement.Generation = "replacement-during-injection"
			data, marshalErr := json.Marshal(replacement)
			if marshalErr != nil {
				return marshalErr
			}
			return os.WriteFile(
				filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock"),
				append(data, '\n'),
				0o600,
			)
		}
		return nil
	}
	err = injectNotification(&wakeConfig{
		me:          "codex",
		root:        root,
		injectMode:  wakeInjectModeRaw,
		controlStop: stop,
	}, "dynamic", false)
	if err != nil {
		t.Fatalf("generation change: %v", err)
	}
	if len(injected) != 1 {
		t.Fatalf("replacement generation received follow-up LF/CR/rescue writes: %#v", injected)
	}
	if current := inspectWakeLock(root, "codex"); current.Lock.Generation != "replacement-during-injection" {
		t.Fatalf("replacement generation changed: %#v", current)
	}
}

func TestCoopRawDoorbellRefusesChangedForegroundTTYBeforeText(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	oldTTY := getWakeCurrentTTY
	const originalTTY = "/dev/ttys111"
	currentTTY := originalTTY
	getWakeCurrentTTY = func() string { return currentTTY }
	t.Cleanup(func() { getWakeCurrentTTY = oldTTY })

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeRaw,
	})
	if err != nil {
		t.Fatalf("acquire wake: %v", err)
	}
	defer cleanup()
	original := inspectWakeLock(root, "codex")
	lock := original.Lock
	lock.TTY = originalTTY
	lockData, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("encode TTY-bound lock: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock"),
		append(lockData, '\n'),
		0o600,
	); err != nil {
		t.Fatalf("write TTY-bound lock: %v", err)
	}
	currentTTY = "/dev/ttys222"

	oldInject := tiocstiInject
	var injected []string
	tiocstiInject = func(text string) error {
		injected = append(injected, text)
		return nil
	}
	t.Cleanup(func() { tiocstiInject = oldInject })
	stop := make(chan struct{})
	err = injectNotification(&wakeConfig{
		me:          "codex",
		root:        root,
		injectMode:  wakeInjectModeRaw,
		controlStop: stop,
	}, "dynamic", false)
	if err != nil {
		t.Fatalf("foreground TTY refusal: %v", err)
	}
	if len(injected) != 0 {
		t.Fatalf("changed foreground TTY received terminal bytes: %#v", injected)
	}
	// The production guard must bind both this exact TTY identity and its
	// foreground process group. A same-path PGRP handoff is the same refusal
	// class and must be rechecked at text, LF, CR, and rescue boundaries.
}

func TestWakeSignalIsCapturedBeforeReadinessPublication(t *testing.T) {
	if os.Getenv(coopWakeSignalHelperEnv) != "" {
		root := os.Getenv("AMQ_TEST_COOP_WAKE_ROOT")
		marker := os.Getenv("AMQ_TEST_COOP_WAKE_MARKER")
		release := os.Getenv("AMQ_TEST_COOP_WAKE_RELEASE")
		ensureCoopWakeMailboxForTest(t, root, "codex")

		cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			wakeMode: wakeInjectModeNone,
		})
		if err != nil {
			t.Fatalf("helper acquire wake: %v", err)
		}
		defer cleanup()
		err = runWakeLoop(wakeConfig{
			root:         root,
			me:           "codex",
			injectMode:   wakeInjectModeNone,
			debounce:     time.Millisecond,
			controlStop:  make(chan struct{}),
			interruptKey: "",
			onPrepared: func(wakeAdmissionWatcher) error {
				if err := os.WriteFile(marker, []byte("entered\n"), 0o600); err != nil {
					return err
				}
				waitForCoopWakePathForTest(t, release, 3*time.Second)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("helper wake loop: %v", err)
		}
		return
	}

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	dir := t.TempDir()
	marker := filepath.Join(dir, "preparing")
	release := filepath.Join(dir, "release")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	cmd := exec.Command(testBinary, "-test.run=^TestWakeSignalIsCapturedBeforeReadinessPublication$")
	cmd.Env = append(os.Environ(),
		coopWakeSignalHelperEnv+"=1",
		"AMQ_TEST_COOP_WAKE_ROOT="+root,
		"AMQ_TEST_COOP_WAKE_MARKER="+marker,
		"AMQ_TEST_COOP_WAKE_RELEASE="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start signal helper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	waitForCoopWakePathForTest(t, marker, 3*time.Second)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal helper before readiness: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(release, []byte("continue\n"), 0o600); err != nil {
		t.Fatalf("release preparation: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		cmd.Process = nil
		if err != nil {
			t.Fatalf("pre-readiness SIGTERM was not handled gracefully: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal helper did not exit")
	}
	if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
		t.Fatalf("pre-readiness signal bypassed exact wake cleanup: %#v", inspection)
	}
}

func TestExactWakeCleanupPreservesReplacementGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeRaw,
	})
	if err != nil {
		t.Fatalf("acquire original wake: %v", err)
	}
	defer cleanup()
	original := inspectWakeLock(root, "codex")
	if !original.Exists || original.Lock.Generation == "" {
		t.Fatalf("original wake = %#v", original)
	}

	replacement := original.Lock
	replacement.Generation = "replacement-generation"
	data, err := json.Marshal(replacement)
	if err != nil {
		t.Fatalf("encode replacement: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock"), append(data, '\n'), 0o600); err != nil {
		t.Fatalf("publish replacement: %v", err)
	}

	if err := cleanupTerminatedWakeLock(original); err != nil {
		t.Fatalf("old generation cleanup: %v", err)
	}
	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.Lock.Generation != replacement.Generation {
		t.Fatalf("old cleanup removed replacement generation: %#v", current)
	}
}

func TestStandaloneWakeRemainsOwnerless(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	t.Setenv(envWakeOwner, "")
	t.Setenv(envWakePrivateStopFD, "")

	loopCalled := false
	err := runWakeWithLoop(
		[]string{
			"--root", root,
			"--me", "codex",
			"--inject-mode", wakeInjectModeNone,
			"--interrupt=false",
		},
		func(cfg wakeConfig) error {
			loopCalled = true
			if cfg.controlStop != nil {
				return errors.New("standalone wake unexpectedly gained a coop owner supervisor")
			}
			inspection := inspectWakeLock(root, "codex")
			if inspection.Lock.Owner != nil || inspection.Lock.OwnerSchema != 0 {
				return fmt.Errorf("standalone wake became owner-bound: %#v", inspection.Lock)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !loopCalled {
		t.Fatal("standalone wake loop was not reached")
	}
}

func TestDarwinCoopWakeUsesConcretePTYOwnerPath(t *testing.T) {
	if os.Getenv(coopWakePTYHelperEnv) != "" {
		if runtime.GOOS != "darwin" {
			t.Skip("Darwin PTY helper")
		}
		if !wakeInputIsTTY() {
			t.Fatal("script helper did not provide a concrete stdin PTY")
		}
		root := os.Getenv("AMQ_TEST_COOP_WAKE_ROOT")
		ensureCoopWakeMailboxForTest(t, root, "codex")
		owner := authoritativeOwnerForPIDForCoopWakeTest(t, os.Getppid())
		encoded, err := encodeWakeOwnerEnv(owner)
		if err != nil {
			t.Fatalf("encode PTY owner: %v", err)
		}
		t.Setenv(envWakeOwner, encoded)
		t.Setenv(envWakePrivateStopFD, "")

		err = runWakeWithLoop(
			[]string{
				"--root", root,
				"--me", "codex",
				"--inject-mode", wakeInjectModeRaw,
				"--interrupt=false",
			},
			func(cfg wakeConfig) error {
				if cfg.controlStop == nil {
					return errors.New("PTY raw wake has no exact owner supervisor")
				}
				return nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin concrete PTY regression")
	}

	root := secureTempDirForTest(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	cmd := exec.Command(
		"/usr/bin/script",
		"-q",
		"/dev/null",
		testBinary,
		"-test.run=^TestDarwinCoopWakeUsesConcretePTYOwnerPath$",
	)
	cmd.Env = append(os.Environ(),
		coopWakePTYHelperEnv+"=1",
		"AMQ_TEST_COOP_WAKE_ROOT="+root,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Darwin PTY coop wake: %v\n%s", err, output)
	}
}

func currentAuthoritativeOwnerForCoopWakeTest(t *testing.T) wakeOwner {
	t.Helper()
	return authoritativeOwnerForPIDForCoopWakeTest(t, os.Getpid())
}

func authoritativeOwnerForPIDForCoopWakeTest(t *testing.T, pid int) wakeOwner {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		proc := inspectWakeProcess(pid)
		sessionID, sessionErr := getWakeProcessSID(pid)
		owner := wakeOwner{
			PID:          pid,
			ProcessStart: proc.StartToken,
			BootID:       proc.BootID,
			SessionID:    sessionID,
		}
		if proc.Running && sessionErr == nil && validateAuthoritativeWakeOwner(owner) == nil {
			return owner
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture authoritative owner pid %d: proc=%#v sid=%d sidErr=%v", pid, proc, sessionID, sessionErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustEncodedCurrentOwnerForCoopWakeTest(t *testing.T) string {
	t.Helper()
	encoded, err := encodeWakeOwnerEnv(currentAuthoritativeOwnerForCoopWakeTest(t))
	if err != nil {
		t.Fatalf("encode current owner: %v", err)
	}
	return encoded
}

func differentProcessStartForCoopWakeTest(current string) string {
	if current == "1" {
		return "2"
	}
	return "1"
}

func ensureCoopWakeMailboxForTest(t *testing.T, root, me string) {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, me); err != nil {
		t.Fatalf("ensure mailbox: %v", err)
	}
}

func waitForCoopWakePathForTest(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPIDForCoopWakeTest(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("parse descendant pid %q: %v", data, err)
	}
	return pid
}
