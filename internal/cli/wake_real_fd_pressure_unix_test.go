//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

func exhaustWakeTestFileDescriptors() (release func(), ok bool) {
	noop := func() {}
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
		return noop, false
	}
	limited := original
	limited.Cur = 96
	if original.Cur < limited.Cur {
		limited.Cur = original.Cur
	}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limited); err != nil {
		return noop, false
	}

	var held []*os.File
	for range 8192 {
		file, err := os.Open(os.DevNull)
		if err != nil {
			break
		}
		held = append(held, file)
	}
	probe, probeErr := os.Open(os.DevNull)
	release = func() {
		for _, file := range held {
			_ = file.Close()
		}
		_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
	}
	if probeErr == nil {
		_ = probe.Close()
		release()
		return noop, false
	}
	if !errors.Is(probeErr, syscall.EMFILE) {
		release()
		return noop, false
	}
	return release, true
}

func writeWakeGenerationForFDPressureTest(
	t *testing.T,
	root, agent, generation string,
) string {
	t.Helper()
	base := fsq.AgentBase(root, agent)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, ".wake.lock")
	if err := os.WriteFile(
		path,
		[]byte(`{"generation":"`+generation+`"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectWakeLockRealEMFILEIsUnreadableNotMissing(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeGenerationForFDPressureTest(t, root, "codex", "real-emfile")
	baseline := inspectWakeLock(root, "codex")
	if !baseline.Exists ||
		baseline.fileInfo == nil ||
		baseline.Lock.Generation != "real-emfile" {
		t.Fatalf("baseline inspection = %+v", baseline)
	}

	release, ok := exhaustWakeTestFileDescriptors()
	if !ok {
		t.Skip("could not materialize file-descriptor exhaustion")
	}
	underPressure := inspectWakeLock(root, "codex")
	release()

	if !underPressure.Exists ||
		underPressure.fileInfo != nil ||
		underPressure.Status != wakeLockUnverified ||
		!strings.Contains(underPressure.Reason, "too many open files") {
		t.Fatalf("real EMFILE inspection shape = %+v", underPressure)
	}
	if after := inspectWakeLock(root, "codex"); !sameWakeLockGeneration(baseline, after) {
		t.Fatal("wake lock changed during file-descriptor pressure")
	}
}

func TestWakeTerminalAuthorityRealEMFILERetriesUnchangedGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeGenerationForFDPressureTest(
		t,
		root,
		"codex",
		"real-authority-emfile",
	)
	baseline := inspectWakeLock(root, "codex")
	tty, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	info, err := tty.Stat()
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := captureWakeTerminalIdentity(info)
	if !ok {
		t.Fatal("capture retained descriptor identity")
	}
	authority := &wakeTerminalAuthority{
		tty:            tty,
		fd:             tty.Fd(),
		identity:       identity,
		foregroundPGRP: 4242,
		generation:     baseline,
		controlStop:    make(chan struct{}),
	}

	release, ok := exhaustWakeTestFileDescriptors()
	if !ok {
		t.Skip("could not materialize file-descriptor exhaustion")
	}
	validationErr := authority.BeforeWrite()
	release()

	var transient *wakeTerminalTransientError
	if !errors.As(validationErr, &transient) ||
		isWakeTerminalAuthorityLoss(validationErr) ||
		classifyWakeFailure(validationErr) != wakeFailureRetry {
		t.Fatalf("real EMFILE validation = %T %v, want retryable transient", validationErr, validationErr)
	}
	if after := inspectWakeLock(root, "codex"); !sameWakeLockGeneration(baseline, after) {
		t.Fatal("wake lock changed during file-descriptor pressure")
	}
}

func TestRetainedWakeInboxRealEMFILERetriesSameDirectory(t *testing.T) {
	_, agentDir := newWakeInboxCapabilityForTest(t)
	inbox, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inbox.Close() })
	if err := inbox.ValidateCanonical(); err != nil {
		t.Fatalf("healthy retained inbox validation: %v", err)
	}

	release, ok := exhaustWakeTestFileDescriptors()
	if !ok {
		t.Skip("could not materialize file-descriptor exhaustion")
	}
	validationErr := inbox.ValidateCanonical()
	release()

	var ownershipLoss *wakeOwnershipLossError
	if validationErr == nil ||
		!errors.Is(validationErr, syscall.EMFILE) ||
		errors.As(validationErr, &ownershipLoss) ||
		classifyWakeFailure(validationErr) != wakeFailureRetry {
		t.Fatalf("real EMFILE inbox validation = %T %v, want retryable transient", validationErr, validationErr)
	}
	if err := inbox.ValidateCanonical(); err != nil {
		t.Fatalf("post-pressure retained inbox validation: %v", err)
	}
}
