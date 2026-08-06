//go:build linux

package cli

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCrashContractLifecycleGuardIsReleasedBeforePidfdWait(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID: wakePID, TTY: "/dev/amq-crash-contract-missing-tty",
		ProcessStart: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
	})
	releasePoll := make(chan struct{})
	inspectCalls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspectCalls++
		if inspectCalls > 1 {
			select {
			case <-releasePoll:
				return wakeProcessInfo{PID: pid, Running: false}
			default:
			}
		}
		return matchingLinuxWakeProcess(pid, root)
	})
	pollEntered := make(chan struct{})
	stubLinuxPidfd(
		t,
		func(int, int) (int, error) { return 99, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error { return nil },
		func(int, time.Duration) (bool, error) {
			close(pollEntered)
			<-releasePoll
			return true, nil
		},
	)

	inspection := inspectWakeLock(root, "codex")
	done := make(chan error, 1)
	go func() {
		_, err := terminateAndRemoveOrphanedWakeLock(inspection)
		done <- err
	}()
	select {
	case <-pollEntered:
	case <-time.After(time.Second):
		t.Fatal("pidfd wait was not reached")
	}

	guardResult := make(chan error, 1)
	go func() {
		guardResult <- withWakeLifecycleGuard(root, "codex", func() error { return nil })
	}()
	select {
	case err := <-guardResult:
		if err != nil {
			t.Fatalf("acquire lifecycle guard during wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pidfd wait retained lifecycle guard")
	}
	close(releasePoll)
	if err := <-done; err != nil {
		t.Fatalf("terminate after releasing pidfd wait: %v", err)
	}
}

func TestCrashContractReloadEndpointRejectsGenerationMismatch(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()

	request := fixture.request()
	request.Generation = "1123456789abcdef0123456789abcdef"
	response, err := sendLinuxWakeReloadTransportRequest(t, fixture, endpoint, request)
	requireLinuxWakeReloadSilentRefusal(t, response, err)
	current := inspectWakeLock(fixture.root, fixture.agent)
	if !sameWakeLockGeneration(fixture.expected, current) {
		t.Fatalf("generation-mismatch request changed lock: %#v", current)
	}
	if err := syscall.Kill(fixture.owner.PID, 0); err != nil {
		t.Fatalf("generation-mismatch request signaled owner: %v", err)
	}
}

func TestCrashContractReloadPublicationFailureLeavesNoEndpoint(t *testing.T) {
	fixture := newLinuxWakeReloadTransportFixture(t)
	path := linuxWakeReloadTransportPath(
		fixture.root,
		fixture.agent,
		fixture.expected.Lock.Generation,
	)
	publicationFailure := errors.New("crash before reload endpoint publication")
	originalBeforePublish := linuxWakeReloadBeforePublish
	linuxWakeReloadBeforePublish = func(int, string, string) error {
		return publicationFailure
	}
	t.Cleanup(func() { linuxWakeReloadBeforePublish = originalBeforePublish })

	endpoint, err := startLinuxWakeReloadTransport(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.expected,
		fixture.owner,
		500*time.Millisecond,
	)
	if endpoint != nil || !errors.Is(err, publicationFailure) {
		t.Fatalf("reload publication failure endpoint=%#v err=%v", endpoint, err)
	}
	var unavailable *wakeReloadTransportUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("reload publication failure was not classified unavailable: %v", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("reload publication failure left public endpoint: %v", statErr)
	}
	entries, readErr := os.ReadDir(fixture.agentDir.path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".wr.stage.") {
			t.Fatalf("reload publication failure left staging endpoint %q", entry.Name())
		}
	}
}
