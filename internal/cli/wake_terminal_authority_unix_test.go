//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"errors"
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

type wakeTerminalInjection struct {
	fd   uintptr
	text string
}

type wakeTerminalAuthorityFixture struct {
	generation     wakeLockInspection
	current        wakeLockInspection
	currentTTYPath string
	foregroundPGRP int
	injections     []wakeTerminalInjection
}

func TestWakeTerminalAuthorityInjectsThroughRetainedFD(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	stop := make(chan struct{})
	authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
	if err != nil {
		t.Fatal(err)
	}
	retainedFD := authority.fd
	t.Cleanup(func() { _ = authority.Close() })

	if err := authority.BeforeWrite(); err != nil {
		t.Fatalf("validate retained terminal authority: %v", err)
	}
	if err := authority.Inject("doorbell"); err != nil {
		t.Fatalf("inject through retained terminal authority: %v", err)
	}
	if len(fixture.injections) != 1 ||
		fixture.injections[0].fd != retainedFD ||
		fixture.injections[0].text != "doorbell" {
		t.Fatalf("retained-fd injections = %#v, want fd=%d text=doorbell", fixture.injections, retainedFD)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestWakeTerminalAuthorityClassifiesUnsupportedOnlyAfterZeroProgressAndRevalidation(t *testing.T) {
	for _, errno := range []error{syscall.EIO, syscall.EPERM} {
		t.Run(errno.Error(), func(t *testing.T) {
			fixture := installWakeTerminalAuthorityFixture(t)
			authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
			if err != nil {
				t.Fatalf("bindWakeTerminalAuthority: %v", err)
			}
			t.Cleanup(func() { _ = authority.Close() })

			injectWakeTerminalFD = func(uintptr, string) error {
				return &tiocstiInjectionError{Err: errno, Progress: 0}
			}
			err = authority.Inject("doorbell")
			var unsupported *wakeInjectorUnsupportedError
			if !errors.As(err, &unsupported) {
				t.Fatalf("Inject error = %T %v, want injector unsupported", err, err)
			}
			if !errors.Is(err, errno) {
				t.Fatalf("Inject error = %v, want wrapped %v", err, errno)
			}
		})
	}
}

func TestWakeTerminalAuthorityEIOWithInvalidCurrentKeepsAuthorityOutcome(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatalf("bindWakeTerminalAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	injectWakeTerminalFD = func(uintptr, string) error {
		fixture.current = wakeLockInspection{}
		return &tiocstiInjectionError{Err: syscall.EIO, Progress: 0}
	}
	err = authority.Inject("doorbell")
	var unsupported *wakeInjectorUnsupportedError
	if errors.As(err, &unsupported) {
		t.Fatalf("invalid current misclassified as injector unsupported: %v", err)
	}
	var authorityLoss *wakeTerminalAuthorityLossError
	if !errors.As(err, &authorityLoss) ||
		!strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("Inject error = %T %v, want generation authority loss", err, err)
	}
}

func TestWakeTerminalAuthorityEIOAfterPartialProgressIsUncertain(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatalf("bindWakeTerminalAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	injectWakeTerminalFD = func(uintptr, string) error {
		return &tiocstiInjectionError{Err: syscall.EIO, Progress: 1}
	}
	err = authority.Inject("doorbell")
	var unsupported *wakeInjectorUnsupportedError
	if errors.As(err, &unsupported) {
		t.Fatalf("partial progress misclassified as injector unsupported: %v", err)
	}
	var authorityLoss *wakeTerminalAuthorityLossError
	if !errors.As(err, &authorityLoss) {
		t.Fatalf("Inject error = %T %v, want uncertain authority outcome", err, err)
	}
}

func TestWakeTerminalAuthorityAllowsSameTerminalCTimeMutation(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	retainedFD := authority.fd

	beforeInfo, err := os.Stat(fixture.currentTTYPath)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := captureWakeFileIdentity(beforeInfo)
	if !ok {
		t.Fatal("capture generic identity before ctime mutation")
	}
	waitForSameFileCTimeMutation(t, fixture.currentTTYPath, beforeInfo, before)

	if err := authority.BeforeWrite(); err != nil {
		t.Fatalf("same-terminal ctime mutation invalidated BeforeWrite: %v", err)
	}
	if err := authority.Inject("doorbell-after-ctime-mutation"); err != nil {
		t.Fatalf("same-terminal ctime mutation invalidated Inject: %v", err)
	}
	if len(fixture.injections) != 1 ||
		fixture.injections[0].fd != retainedFD ||
		fixture.injections[0].text != "doorbell-after-ctime-mutation" {
		t.Fatalf("ctime-mutation injections = %#v, want fd=%d and exact payload", fixture.injections, retainedFD)
	}
}

func TestWakeTerminalAuthorityDarwinPTYCTimeMutation(t *testing.T) {
	const helperEnv = "AMQ_TEST_WAKE_TERMINAL_IDENTITY_PTY"
	if os.Getenv(helperEnv) == "1" {
		if runtime.GOOS != "darwin" {
			t.Skip("Darwin PTY helper")
		}

		realOpen := openWakeControllingTerminal
		realPGRP := wakeTerminalForegroundPGRP
		fixture := installWakeTerminalAuthorityFixture(t)
		openWakeControllingTerminal = realOpen
		wakeTerminalForegroundPGRP = realPGRP
		var injections []wakeTerminalInjection
		injectWakeTerminalFD = func(fd uintptr, text string) error {
			injections = append(injections, wakeTerminalInjection{fd: fd, text: text})
			return nil
		}

		authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
		if err != nil {
			t.Fatalf("bind real PTY authority: %v", err)
		}
		t.Cleanup(func() { _ = authority.Close() })
		originalState := strings.TrimSpace(runDarwinTTYMutation(t, "-g"))
		originalRows, originalCols := readDarwinTTYSize(t)
		t.Cleanup(func() {
			if output, err := exec.Command(
				"/bin/stty",
				"-f",
				"/dev/tty",
				originalState,
			).CombinedOutput(); err != nil {
				t.Errorf("restore tty state: %v\n%s", err, output)
			}
			if output, err := exec.Command(
				"/bin/stty",
				"-f",
				"/dev/tty",
				"rows",
				strconv.Itoa(originalRows),
				"cols",
				strconv.Itoa(originalCols),
			).CombinedOutput(); err != nil {
				t.Errorf("restore tty size: %v\n%s", err, output)
			}
		})

		if err := authority.Inject("before-mutation"); err != nil {
			t.Fatalf("inject before real PTY mutation: %v", err)
		}

		beforeInfo, err := authority.tty.Stat()
		if err != nil {
			t.Fatal(err)
		}
		before, ok := captureWakeFileIdentity(beforeInfo)
		if !ok {
			t.Fatal("capture real PTY identity before mutation")
		}
		if _, err := authority.tty.Write([]byte("tty identity probe\r\n")); err != nil {
			t.Fatalf("generate PTY traffic: %v", err)
		}
		runDarwinTTYMutation(t, "-echo")
		runDarwinTTYMutation(t, "echo")
		if err := authority.Inject("after-traffic-and-termios"); err != nil {
			t.Fatalf("inject after PTY traffic and termios: %v", err)
		}

		resizeRows := originalRows + 1
		resizeCols := originalCols + 1
		runDarwinTTYMutation(
			t,
			"rows",
			strconv.Itoa(resizeRows),
			"cols",
			strconv.Itoa(resizeCols),
		)
		actualRows, actualCols := readDarwinTTYSize(t)
		if actualRows != resizeRows || actualCols != resizeCols {
			t.Fatalf(
				"PTY resize = %dx%d, want changed %dx%d from %dx%d",
				actualRows,
				actualCols,
				resizeRows,
				resizeCols,
				originalRows,
				originalCols,
			)
		}

		afterInfo, err := authority.tty.Stat()
		if err != nil {
			t.Fatal(err)
		}
		after, ok := captureWakeFileIdentity(afterInfo)
		if !ok {
			t.Fatal("capture real PTY identity after mutation")
		}
		if before.Device != after.Device || before.Inode != after.Inode {
			t.Fatalf("real PTY Dev/Ino changed: before=%+v after=%+v", before, after)
		}
		if matchesWakeFileIdentity(before, afterInfo) {
			t.Fatalf("real PTY traffic/termios/resize did not change ctime: before=%+v after=%+v", before, after)
		}
		if err := authority.BeforeWrite(); err != nil {
			t.Fatalf("real PTY mutation invalidated BeforeWrite: %v", err)
		}
		if err := authority.Inject("after-resize"); err != nil {
			t.Fatalf("real PTY mutation invalidated Inject: %v", err)
		}
		wantInjections := []wakeTerminalInjection{
			{fd: authority.fd, text: "before-mutation"},
			{fd: authority.fd, text: "after-traffic-and-termios"},
			{fd: authority.fd, text: "after-resize"},
		}
		if len(injections) != len(wantInjections) {
			t.Fatalf("real PTY injections = %#v, want %#v", injections, wantInjections)
		}
		for i := range wantInjections {
			if injections[i] != wantInjections[i] {
				t.Fatalf("real PTY injection %d = %#v, want %#v", i, injections[i], wantInjections[i])
			}
		}
		return
	}
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin PTY regression")
	}
	if _, err := os.Stat("/usr/bin/script"); err != nil {
		t.Skipf("Darwin PTY regression requires /usr/bin/script: %v", err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := os.CreateTemp(t.TempDir(), "amq-tty-identity-hotfix-pty-*.log")
	if err != nil {
		t.Fatalf("create owned PTY evidence: %v", err)
	}
	evidencePath := evidence.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"/usr/bin/script",
		"-q",
		"/dev/null",
		testBinary,
		"-test.run=^TestWakeTerminalAuthorityDarwinPTYCTimeMutation$",
	)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Run()
	if _, writeErr := evidence.Write(output.Bytes()); writeErr != nil {
		_ = evidence.Close()
		t.Fatalf("write owned PTY evidence %s: %v", evidencePath, writeErr)
	}
	if syncErr := evidence.Sync(); syncErr != nil {
		_ = evidence.Close()
		t.Fatalf("sync owned PTY evidence %s: %v", evidencePath, syncErr)
	}
	if closeErr := evidence.Close(); closeErr != nil {
		t.Fatalf("close owned PTY evidence %s: %v", evidencePath, closeErr)
	}
	t.Logf("PTY evidence: %s", evidencePath)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Darwin real PTY identity regression timed out; evidence=%s\n%s", evidencePath, output.String())
	}
	if err != nil {
		t.Fatalf("Darwin real PTY identity regression: %v; evidence=%s\n%s", err, evidencePath, output.String())
	}
}

func waitForSameFileCTimeMutation(
	t *testing.T,
	path string,
	beforeInfo os.FileInfo,
	before wakeFileIdentity,
) {
	t.Helper()
	originalMode := beforeInfo.Mode().Perm()
	t.Cleanup(func() {
		if err := os.Chmod(path, originalMode); err != nil {
			t.Errorf("restore ctime fixture mode: %v", err)
		}
	})
	alternateMode := originalMode ^ 0o040
	deadline := time.Now().Add(500 * time.Millisecond)
	mode := alternateMode
	for {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		afterInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		after, ok := captureWakeFileIdentity(afterInfo)
		if !ok {
			t.Fatal("capture generic identity after ctime mutation")
		}
		if before.Device != after.Device || before.Inode != after.Inode {
			t.Fatalf("ctime mutation replaced file: before=%+v after=%+v", before, after)
		}
		if !matchesWakeFileIdentity(before, afterInfo) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("generic wakeFileIdentity did not observe bounded ctime mutation: before=%+v", before)
		}
		if mode == alternateMode {
			mode = originalMode
		} else {
			mode = alternateMode
		}
		time.Sleep(time.Millisecond)
	}
}

func runDarwinTTYMutation(t *testing.T, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-f", "/dev/tty"}, args...)
	output, err := exec.Command("/bin/stty", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("stty %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func readDarwinTTYSize(t *testing.T) (int, int) {
	t.Helper()
	fields := strings.Fields(runDarwinTTYMutation(t, "size"))
	if len(fields) != 2 {
		t.Fatalf("stty size = %q, want rows and columns", fields)
	}
	rows, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("parse tty rows %q: %v", fields[0], err)
	}
	cols, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parse tty cols %q: %v", fields[1], err)
	}
	return rows, cols
}

func TestWakeTerminalAuthorityRefusesSamePathForegroundPGRPHandoff(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	fixture.foregroundPGRP++
	err = authority.Inject("must-not-arrive")
	if !isWakeTerminalAuthorityLoss(err) ||
		!isWakeTerminalForegroundPGRPChanged(err) ||
		!strings.Contains(err.Error(), "foreground process group changed") {
		t.Fatalf("same-path foreground-pgrp handoff error = %v", err)
	}
	if len(fixture.injections) != 0 {
		t.Fatalf("same-path foreground-pgrp handoff injected: %#v", fixture.injections)
	}
}

func TestWakeTerminalAuthorityRefusesChangedRetainedFDIdentity(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	replacementPath := filepath.Join(t.TempDir(), "replacement-tty")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authority.tty.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.OpenFile(replacementPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	authority.tty = replacement
	fixture.currentTTYPath = replacementPath

	err = authority.Inject("must-not-arrive")
	if !isWakeTerminalAuthorityLoss(err) ||
		(!strings.Contains(err.Error(), "descriptor changed") &&
			!strings.Contains(err.Error(), "identity changed")) {
		t.Fatalf("changed retained-fd identity error = %v", err)
	}
	if len(fixture.injections) != 0 {
		t.Fatalf("changed retained-fd identity injected: %#v", fixture.injections)
	}
}

func TestWakeTerminalAuthorityRefusesChangedCurrentTTYIdentity(t *testing.T) {
	fixture := installWakeTerminalAuthorityFixture(t)
	authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	replacementPath := filepath.Join(t.TempDir(), "replacement-current-tty")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(replacementPath)
	if err != nil {
		t.Fatal(err)
	}
	replacement, ok := captureWakeFileIdentity(replacementInfo)
	if !ok {
		t.Fatal("capture replacement terminal identity")
	}
	originalInfo, err := authority.tty.Stat()
	if err != nil {
		t.Fatal(err)
	}
	original, ok := captureWakeFileIdentity(originalInfo)
	if !ok {
		t.Fatal("capture original terminal identity")
	}
	if original.Device == replacement.Device && original.Inode == replacement.Inode {
		t.Fatalf("replacement did not change terminal Dev/Ino: original=%+v replacement=%+v", original, replacement)
	}
	fixture.currentTTYPath = replacementPath

	err = authority.Inject("must-not-arrive")
	if !isWakeTerminalAuthorityLoss(err) ||
		!strings.Contains(err.Error(), "current controlling-terminal identity changed") {
		t.Fatalf("changed current tty identity error = %v", err)
	}
	if len(fixture.injections) != 0 {
		t.Fatalf("changed current tty identity injected: %#v", fixture.injections)
	}
}

func TestWakeTerminalAuthorityRefusesChangedGenerationAndStoppedControl(t *testing.T) {
	t.Run("generation", func(t *testing.T) {
		fixture := installWakeTerminalAuthorityFixture(t)
		authority, err := bindWakeTerminalAuthority(fixture.generation, make(chan struct{}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })

		fixture.current.Lock.Generation = "replacement-generation"
		err = authority.Inject("must-not-arrive")
		if !isWakeTerminalAuthorityLoss(err) ||
			!strings.Contains(err.Error(), "wake generation changed") {
			t.Fatalf("changed generation error = %v", err)
		}
		if len(fixture.injections) != 0 {
			t.Fatalf("changed generation injected: %#v", fixture.injections)
		}
	})

	t.Run("control stopped", func(t *testing.T) {
		fixture := installWakeTerminalAuthorityFixture(t)
		stop := make(chan struct{})
		authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })

		close(stop)
		err = authority.Inject("must-not-arrive")
		var loss *wakeTerminalAuthorityLossError
		if !errors.As(err, &loss) || !strings.Contains(loss.Reason, "control stopped") {
			t.Fatalf("closed control-stop error = %v", err)
		}
		if len(fixture.injections) != 0 {
			t.Fatalf("closed control-stop injected: %#v", fixture.injections)
		}
	})
}

func TestWakeTerminalControlStoppedClassificationIsStructural(t *testing.T) {
	stopped := newWakeTerminalControlStoppedLoss()
	if !isWakeTerminalControlStopped(stopped) {
		t.Fatalf("typed stopped-control loss was not classified: %v", stopped)
	}

	sameReasonOnly := newWakeTerminalAuthorityLoss("wake control stopped", nil)
	if isWakeTerminalControlStopped(sameReasonOnly) {
		t.Fatalf("reason-only authority loss was classified as stopped control: %v", sameReasonOnly)
	}
	if !isWakeTerminalAuthorityLoss(sameReasonOnly) {
		t.Fatalf("reason-only loss stopped being an authority loss: %v", sameReasonOnly)
	}
}

func TestWakeTerminalAuthorityStopClosingAtWriteBoundariesIsNonfatal(t *testing.T) {
	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}
	t.Cleanup(func() {
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})

	t.Run("between outer stop check and before-write validation", func(t *testing.T) {
		fixture := installWakeTerminalAuthorityFixture(t)
		stop := make(chan struct{})
		authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })

		beforeCalls := 0
		err = injectNotification(&wakeConfig{
			me:          "codex",
			injectMode:  wakeInjectModeRaw,
			controlStop: stop,
			beforeTerminalWrite: func() error {
				beforeCalls++
				close(stop)
				return authority.BeforeWrite()
			},
			terminalWrite: authority.Inject,
		}, "dynamic", false)
		if err != nil {
			t.Fatalf("stop before retained validation: %v", err)
		}
		if beforeCalls != 1 {
			t.Fatalf("before-write calls = %d, want 1", beforeCalls)
		}
		if len(fixture.injections) != 0 {
			t.Fatalf("stopped authority injected terminal bytes: %#v", fixture.injections)
		}
	})

	t.Run("between before-write validation and retained inject", func(t *testing.T) {
		fixture := installWakeTerminalAuthorityFixture(t)
		stop := make(chan struct{})
		authority, err := bindWakeTerminalAuthority(fixture.generation, stop)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })

		beforeCalls := 0
		injectCalls := 0
		err = injectNotification(&wakeConfig{
			me:          "codex",
			injectMode:  wakeInjectModeRaw,
			controlStop: stop,
			beforeTerminalWrite: func() error {
				beforeCalls++
				if err := authority.BeforeWrite(); err != nil {
					return err
				}
				close(stop)
				return nil
			},
			terminalWrite: func(chunk string) error {
				injectCalls++
				return authority.Inject(chunk)
			},
		}, "dynamic", false)
		if err != nil {
			t.Fatalf("stop before retained inject: %v", err)
		}
		if beforeCalls != 1 || injectCalls != 1 {
			t.Fatalf("boundary calls before=%d inject=%d, want 1/1", beforeCalls, injectCalls)
		}
		if len(fixture.injections) != 0 {
			t.Fatalf("stopped authority injected terminal bytes: %#v", fixture.injections)
		}
	})
}

func TestRunWakeLoopTerminatesOnTerminalAuthorityLoss(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "terminal-authority-loss",
			From:    "sender",
			To:      []string{"codex"},
			Thread:  "p2p/sender__codex",
			Subject: "wake",
			Created: time.Now().UTC().Format(time.RFC3339),
		},
		Body: "durable body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	loss := newWakeTerminalAuthorityLoss("test authority loss", nil)
	terminalWriteCalled := false
	err = runWakeLoop(wakeConfig{
		root:        root,
		me:          "codex",
		injectMode:  wakeInjectModeRaw,
		controlStop: make(chan struct{}),
		beforeTerminalWrite: func() error {
			return loss
		},
		terminalWrite: func(string) error {
			terminalWriteCalled = true
			return nil
		},
		onPrepared: func(wakeAdmissionWatcher) error {
			_, deliverErr := deliverToInboxForTest(
				t,
				root,
				"codex",
				"terminal-authority-loss.md",
				data,
			)
			return deliverErr
		},
	})
	if !isWakeTerminalAuthorityLoss(err) || !errors.Is(err, loss) {
		t.Fatalf("wake loop authority-loss result = %v", err)
	}
	if isWakeTerminalForegroundPGRPChanged(err) {
		t.Fatalf("generic authority loss classified as foreground-pgrp change: %v", err)
	}
	if terminalWriteCalled {
		t.Fatal("wake loop wrote after terminal authority loss")
	}
}

func TestRunWakeLoopHoldsNotificationDuringForegroundPGRPMismatch(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "foreground-pgrp-mismatch",
			From:    "sender",
			To:      []string{"codex"},
			Thread:  "p2p/sender__codex",
			Subject: "wake",
			Created: time.Now().UTC().Format(time.RFC3339),
		},
		Body: "durable body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	messagePath := filepath.Join(
		fsq.AgentInboxNew(root, "codex"),
		"foreground-pgrp-mismatch.md",
	)
	if err := os.WriteFile(messagePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	firstRefused := make(chan struct{}, 1)
	restored := make(chan struct{})
	delivered := make(chan struct{}, 1)
	controlStop := make(chan struct{})
	runDone := make(chan error, 1)

	go func() {
		runDone <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			injectMode:  wakeInjectModeRaw,
			controlStop: controlStop,
			beforeTerminalWrite: func() error {
				select {
				case <-restored:
					return nil
				default:
					select {
					case firstRefused <- struct{}{}:
					default:
					}
					return newWakeTerminalForegroundPGRPChangedLoss(101, 202)
				}
			},
			terminalWrite: func(string) error {
				select {
				case delivered <- struct{}{}:
				default:
				}
				return nil
			},
		})
	}()

	select {
	case <-firstRefused:
	case err := <-runDone:
		t.Fatalf("wake loop exited on foreground-pgrp mismatch: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not attempt held notification")
	}

	select {
	case <-delivered:
		t.Fatal("wake loop wrote while foreground pgrp mismatched")
	case err := <-runDone:
		t.Fatalf("wake loop exited while foreground pgrp mismatched: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(restored)
	select {
	case <-delivered:
	case err := <-runDone:
		t.Fatalf("wake loop exited before delivering held notification: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("held notification was not delivered after foreground pgrp restored")
	}

	close(controlStop)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("wake loop after restored delivery: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not stop")
	}
}

func installWakeTerminalAuthorityFixture(t *testing.T) *wakeTerminalAuthorityFixture {
	t.Helper()
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".wake.lock")
	lockRaw := []byte(`{"generation":"terminal-authority-generation"}`)
	if err := os.WriteFile(lockPath, lockRaw, 0o400); err != nil {
		t.Fatal(err)
	}
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	ttyPath := filepath.Join(dir, "tty")
	if err := os.WriteFile(ttyPath, []byte("tty"), 0o600); err != nil {
		t.Fatal(err)
	}
	generation := wakeLockInspection{
		Exists:   true,
		Root:     dir,
		Agent:    "codex",
		LockPath: lockPath,
		Lock: wakeLock{
			Generation: "terminal-authority-generation",
		},
		raw:      lockRaw,
		fileInfo: lockInfo,
	}
	fixture := &wakeTerminalAuthorityFixture{
		generation:     generation,
		current:        generation,
		currentTTYPath: ttyPath,
		foregroundPGRP: 4242,
	}

	originalOpen := openWakeControllingTerminal
	originalPGRP := wakeTerminalForegroundPGRP
	originalInspect := inspectWakeTerminalGeneration
	originalInject := injectWakeTerminalFD
	openWakeControllingTerminal = func() (*os.File, error) {
		return os.OpenFile(fixture.currentTTYPath, os.O_RDWR, 0)
	}
	wakeTerminalForegroundPGRP = func(uintptr) (int, error) {
		return fixture.foregroundPGRP, nil
	}
	inspectWakeTerminalGeneration = func(root, agent string) wakeLockInspection {
		if root != generation.Root || agent != generation.Agent {
			t.Fatalf("inspect generation root=%q agent=%q", root, agent)
		}
		return fixture.current
	}
	injectWakeTerminalFD = func(fd uintptr, text string) error {
		fixture.injections = append(fixture.injections, wakeTerminalInjection{fd: fd, text: text})
		return nil
	}
	t.Cleanup(func() {
		openWakeControllingTerminal = originalOpen
		wakeTerminalForegroundPGRP = originalPGRP
		inspectWakeTerminalGeneration = originalInspect
		injectWakeTerminalFD = originalInject
	})
	return fixture
}
