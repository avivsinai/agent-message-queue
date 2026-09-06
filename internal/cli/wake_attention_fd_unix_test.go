//go:build darwin || linux

package cli

import (
	"errors"
	"io"
	"os"
	"strconv"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

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
