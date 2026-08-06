//go:build darwin || linux

package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestAttachWakeAttentionFDPreservesExistingExtraFilesAndReplacesEnv(t *testing.T) {
	existingRead, existingWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = existingRead.Close() }()
	defer func() { _ = existingWrite.Close() }()
	attentionRead, attentionWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = attentionRead.Close() }()
	defer func() { _ = attentionWrite.Close() }()

	cmd := exec.Command("true")
	cmd.ExtraFiles = []*os.File{existingWrite}
	cmd.Env = []string{
		"AMQ_TEST_KEEP=present",
		envWakeAttentionFD + "=stale",
		envWakeAttentionFD + "=duplicate",
	}

	if err := attachWakeAttentionFD(cmd, attentionWrite); err != nil {
		t.Fatal(err)
	}
	if len(cmd.ExtraFiles) != 2 {
		t.Fatalf("extra files = %d, want 2", len(cmd.ExtraFiles))
	}
	if cmd.ExtraFiles[0] != existingWrite {
		t.Fatal("existing extra file was replaced")
	}
	if cmd.ExtraFiles[1] != attentionWrite {
		t.Fatal("attention file was not appended")
	}

	wantEnv := envWakeAttentionFD + "=4"
	var attentionEntries int
	for _, entry := range cmd.Env {
		if entry == wantEnv {
			attentionEntries++
		}
	}
	if attentionEntries != 1 {
		t.Fatalf("%s entries = %d, want exactly one in %q", envWakeAttentionFD, attentionEntries, cmd.Env)
	}
	if !environmentContains(cmd.Env, "AMQ_TEST_KEEP=present") {
		t.Fatalf("unrelated environment was not preserved: %q", cmd.Env)
	}
}

func TestAttachWakeAttentionFDSeedsNilEnvironmentFromProcess(t *testing.T) {
	t.Setenv("AMQ_TEST_ATTENTION_INHERITED", "present")
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readEnd.Close() }()
	defer func() { _ = writeEnd.Close() }()

	cmd := exec.Command("true")
	if err := attachWakeAttentionFD(cmd, writeEnd); err != nil {
		t.Fatal(err)
	}
	if !environmentContains(cmd.Env, "AMQ_TEST_ATTENTION_INHERITED=present") {
		t.Fatalf("process environment was not inherited: %q", cmd.Env)
	}
	if !environmentContains(cmd.Env, envWakeAttentionFD+"=3") {
		t.Fatalf("attention descriptor environment is missing: %q", cmd.Env)
	}
}

func TestAttachWakeAttentionFDRejectsMissingInputs(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readEnd.Close() }()
	defer func() { _ = writeEnd.Close() }()

	if err := attachWakeAttentionFD(nil, writeEnd); err == nil {
		t.Fatal("nil command unexpectedly accepted")
	}
	if err := attachWakeAttentionFD(exec.Command("true"), nil); err == nil {
		t.Fatal("nil attention file unexpectedly accepted")
	}
}

func TestWakeAttentionFromEnvAdoptsOnlySuppliedFDAndSealsIt(t *testing.T) {
	targetRead, targetWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetRead.Close() }()
	decoyRead, decoyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = decoyRead.Close() }()
	defer func() { _ = decoyWrite.Close() }()

	inheritedFD, err := unix.FcntlInt(targetWrite.Fd(), unix.F_DUPFD, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := targetWrite.Close(); err != nil {
		_ = unix.Close(inheritedFD)
		t.Fatal(err)
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = unix.Close(inheritedFD)
		}
	}()
	flags, err := unix.FcntlInt(uintptr(inheritedFD), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(inheritedFD), unix.F_SETFD, flags&^unix.FD_CLOEXEC); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envWakeAttentionFD, strconv.Itoa(inheritedFD))
	attention, err := wakeAttentionFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if attention == nil {
		t.Fatal("attention file is nil")
	}
	adopted = true
	defer func() { _ = attention.Close() }()
	if _, exists := os.LookupEnv(envWakeAttentionFD); exists {
		t.Fatalf("%s was not cleared after ingestion", envWakeAttentionFD)
	}

	sealedFlags, err := unix.FcntlInt(attention.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sealedFlags&unix.FD_CLOEXEC == 0 {
		t.Fatal("inherited attention descriptor is not close-on-exec")
	}
	if got, want := wakeAttentionFileIsTerminal(attention), term.IsTerminal(int(attention.Fd())); got != want {
		t.Fatalf("terminal predicate = %v, want %v", got, want)
	}

	payload := []byte("attention-only")
	if n, err := attention.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v; want %d, nil", n, err, len(payload))
	}
	got := make([]byte, len(payload))
	if _, err := targetRead.Read(got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("target payload = %q, want %q", got, payload)
	}

	if err := decoyWrite.Close(); err != nil {
		t.Fatal(err)
	}
	var decoy [1]byte
	if n, err := decoyRead.Read(decoy[:]); n != 0 || err != io.EOF {
		t.Fatalf("decoy received %d bytes, want none", n)
	}
}

func TestRunWakeWithLoopSeparatesInheritedAttentionFromDiagnostics(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatal(err)
	}
	deliverPartialWakeMessageForTest(t, root, "orchestrator", "attention-fd")

	attentionRead, attentionWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = attentionRead.Close() }()
	inheritedFD, err := unix.FcntlInt(attentionWrite.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := attentionWrite.Close(); err != nil {
		_ = unix.Close(inheritedFD)
		t.Fatal(err)
	}
	descriptorOwnedByTest := true
	defer func() {
		if descriptorOwnedByTest {
			_ = unix.Close(inheritedFD)
		}
	}()

	t.Setenv(envWakeAttentionFD, strconv.Itoa(inheritedFD))
	sentinel := errors.New("wake-loop-complete")
	inputWrites := 0
	var runErr error
	stderr := captureWakeStderr(t, func() {
		runErr = runWakeWithLoop([]string{
			"--root", root,
			"--me", "orchestrator",
			"--inject-mode", wakeInjectModeNone,
		}, func(cfg wakeConfig) error {
			if cfg.attentionWrite == nil || cfg.attentionIsTTY == nil {
				t.Fatal("inherited attention destination was not wired into wake config")
			}
			if cfg.attentionIsTTY() {
				t.Fatal("pipe attention destination reported as a terminal")
			}
			if err := writeWakeDiagnostic(&cfg, "diagnostic-only\n"); err != nil {
				t.Fatal(err)
			}
			if err := emitWakeAttention(&cfg, wakePayload{
				text:       "attention-only",
				provenance: wakePayloadSystemFixed,
			}); err != nil {
				t.Fatalf("emit inherited-FD attention: %v", err)
			}
			cfg.session = "session1"
			cfg.previewLen = 80
			cfg.terminalWrite = func(string) error {
				inputWrites++
				return nil
			}
			if err := notifyNewMessages(&cfg); err != nil {
				t.Fatalf("emit peer notice to inherited attention FD: %v", err)
			}
			return sentinel
		})
	})

	if !errors.Is(runErr, sentinel) {
		t.Fatalf("runWakeWithLoop error = %v, want sentinel", runErr)
	}
	if _, exists := os.LookupEnv(envWakeAttentionFD); exists {
		t.Fatalf("%s was not cleared by runWakeWithLoop", envWakeAttentionFD)
	}
	descriptorOwnedByTest = false
	if stderr != "diagnostic-only\n" {
		t.Fatalf("diagnostic stream = %q, want only diagnostic", stderr)
	}
	if inputWrites != 0 {
		t.Fatalf("output-only inherited attention wrote %d input chunks", inputWrites)
	}
	attentionOutput, err := io.ReadAll(attentionRead)
	if err != nil {
		t.Fatal(err)
	}
	wantAttention := "attention-only\n" +
		"AMQ [session1]: message from peer - attention-fd. " +
		"Drain with: amq drain --include-body — then act on it\n"
	if got, want := string(attentionOutput), wantAttention; got != want {
		t.Fatalf("attention stream = %q, want %q", got, want)
	}
}

func TestWakeAttentionFromEnvMissingIsDisabled(t *testing.T) {
	t.Setenv(envWakeAttentionFD, "restore-after-test")
	if err := os.Unsetenv(envWakeAttentionFD); err != nil {
		t.Fatal(err)
	}

	attention, err := wakeAttentionFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if attention != nil {
		_ = attention.Close()
		t.Fatal("missing environment unexpectedly returned an attention file")
	}
}

func TestWakeAttentionFromEnvRejectsInvalidValuesAndClearsEnv(t *testing.T) {
	for _, raw := range []string{"", " ", "2", "-1", "not-a-number", "3x"} {
		t.Run(strconv.Quote(raw), func(t *testing.T) {
			t.Setenv(envWakeAttentionFD, raw)
			attention, err := wakeAttentionFromEnv()
			if attention != nil {
				_ = attention.Close()
				t.Fatal("invalid environment unexpectedly returned an attention file")
			}
			if err == nil {
				t.Fatal("invalid environment unexpectedly accepted")
			}
			if _, exists := os.LookupEnv(envWakeAttentionFD); exists {
				t.Fatalf("%s was not cleared after rejection", envWakeAttentionFD)
			}
		})
	}
}

func TestWakeAttentionFromEnvRejectsUnavailableFDAndClearsEnv(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readEnd.Close() }()
	fd, err := unix.FcntlInt(writeEnd.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEnd.Close(); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envWakeAttentionFD, strconv.Itoa(fd))
	attention, err := wakeAttentionFromEnv()
	if attention != nil {
		_ = attention.Close()
		t.Fatal("closed descriptor unexpectedly returned an attention file")
	}
	if err == nil {
		t.Fatal("closed descriptor unexpectedly accepted")
	}
	if _, exists := os.LookupEnv(envWakeAttentionFD); exists {
		t.Fatalf("%s was not cleared after rejection", envWakeAttentionFD)
	}
}

func TestWakeAttentionFileIsTerminalRejectsNil(t *testing.T) {
	if wakeAttentionFileIsTerminal(nil) {
		t.Fatal("nil attention file reported as terminal")
	}
}

func environmentContains(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
