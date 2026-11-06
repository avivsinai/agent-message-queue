//go:build darwin

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func stubDarwinProcessExecutablePathErrno(t *testing.T, errno syscall.Errno) {
	t.Helper()
	previous := readDarwinProcessExecutablePath
	readDarwinProcessExecutablePath = func(pid int) (string, error) {
		return "", fmt.Errorf("proc_pidpath pid %d: %w", pid, errno)
	}
	t.Cleanup(func() { readDarwinProcessExecutablePath = previous })
}

// agent-message-queue-5bu: after brew upgrade removed the old Cellar image,
// proc_pidpath returned ENOENT for the live wake and self-upgrade deferred on
// every tick with "live wake image identity is unknown or ambiguous".
func TestMaintainWakeSelfUpgradeUpgradesWakeWhoseImageWasDeleted(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
	state := selfUpgradeStateForCandidate(t, candidate)
	stubWakeSelfUpgradeVersion(t, "0.57.0")
	stubDarwinProcessExecutablePathErrno(t, unix.ENOENT)

	decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
	if err != nil || decision.Action != wakeSelfUpgradeActionPending {
		t.Fatalf("deleted-image decision=%#v err=%v, want %s", decision, err, wakeSelfUpgradeActionPending)
	}
}

// agent-message-queue-5bu: the deleted-image path must not give authority to
// a process that is gone or that does not map the recorded image.
func TestDarwinWakeBinaryComparisonRefusesUnprovenDeletedImage(t *testing.T) {
	path, err := readDarwinProcessExecutablePath(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(foreign, []byte("not this process"), 0o700); err != nil {
		t.Fatal(err)
	}
	foreignInfo, err := os.Stat(foreign)
	if err != nil {
		t.Fatal(err)
	}
	recorded := darwinWakeImageEvidenceForTest(t, foreign, foreignInfo)

	for name, tc := range map[string]struct {
		errno syscall.Errno
		want  string
	}{
		"process gone":                      {unix.ESRCH, "resolve wake process executable"},
		"recorded image not in the process": {unix.ENOENT, "found 0 candidates"},
	} {
		t.Run(name, func(t *testing.T) {
			stubDarwinProcessExecutablePathErrno(t, tc.errno)
			got, err := inspectWakeBinaryStalenessPlatform(
				wakeLockInspection{
					PID: os.Getpid(),
					Lock: wakeLock{
						Started:              time.Now().UTC().Format(time.RFC3339),
						ImagePath:            foreign,
						ImageVersion:         recorded.EmbeddedVersion,
						RunningImageEvidence: &recorded,
					},
				},
				resolvedWakeBinary{Path: path, Info: currentInfo},
			)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("comparison = %#v err=%v, want refusal containing %q", got, err, tc.want)
			}
		})
	}
}

// agent-message-queue-5bu: `amq wake restart` from the agent's Bash tool was
// refused live. The harness runs that tool in a new session under the owner
// process, and the wake accepted only the owner's own session.
func TestDarwinWakeRestartControlAcceptsPeerUnderOwnerInAnotherSession(t *testing.T) {
	root, agent, _, lock, peerSession := testDarwinWakeRestartControlLock(t)
	parent := inspectWakeProcessPlatform(os.Getppid())
	if !parent.Running || parent.StartToken == "" || parent.BootID == "" {
		t.Fatalf("test parent identity = %#v", parent)
	}
	owner := wakeOwner{
		PID:          parent.PID,
		ProcessStart: parent.StartToken,
		BootID:       parent.BootID,
		SessionID:    99,
	}
	lock.ResumeOwner = &owner
	configureWakeRestartAdvertisementPlatform(&lock, root, agent)
	writeWakeLockForTest(t, root, agent, lock)
	previousObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
		return wakeOwnerObservation{State: wakeOwnerSame, Reason: "test owner evidence"}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = previousObserve })
	*peerSession = owner.SessionID + 1
	record := writeDarwinWakeRestartRecordForTest(t, root, agent, owner, lock)

	restartSignals := make(chan os.Signal, 1)
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	cleanup, _, _, err := startWakeControlListenerInDirWithRestart(agentDir, root, agent, lock, restartSignals, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	response := sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
		Generation: record.Generation,
		RequestID:  record.RequestID,
		Owner:      &owner,
		Operation:  wakeControlRestartOperation,
	})
	if response != "ACK" {
		t.Fatalf("restart response = %q, want ACK", response)
	}
	select {
	case <-restartSignals:
	default:
		t.Fatal("accepted restart did not queue its signal")
	}
}

// agent-message-queue-5bu: a refused `amq wake restart` left its request
// pending, so wake check reported reload_restart_pending and self-upgrade
// stopped at "an existing wake restart request is preserved".
func TestMaintainWakeSelfUpgradeProceedsAfterUnfinishableRestartRequest(t *testing.T) {
	for name, leave := range map[string]func(t *testing.T, fixture wakeRestartFixture){
		"refused by the wake": func(t *testing.T, fixture wakeRestartFixture) {
			previousObserve := observeAuthoritativeWakeOwner
			observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
				return wakeOwnerObservation{State: wakeOwnerUnknown, Reason: "test owner unknown"}, nil
			}
			defer func() { observeAuthoritativeWakeOwner = previousObserve }()
			cleanup, _, _, err := startWakeControlListenerInDirWithRestart(
				fixture.agentDir, fixture.root, fixture.agent, fixture.lock.Lock, make(chan os.Signal, 1), nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			response := sendDarwinOwnerControlRequest(t, fixture.root, fixture.agent, fixture.lock.Lock, wakeControlOwnerRequest{
				Generation: fixture.record.Generation,
				RequestID:  fixture.record.RequestID,
				Owner:      &fixture.record.Owner,
				Operation:  wakeControlRestartOperation,
			})
			if !strings.Contains(response, "test owner unknown") {
				t.Fatalf("restart response = %q, want the refusal reason", response)
			}
		},
		"requester exited": func(t *testing.T, fixture wakeRestartFixture) {
			requester := fixture.owner
			requester.PID++
			record := fixture.record
			record.Requester = &requester
			if err := withWakeMutationScopeInDir(fixture.agentDir, wakeGuardWait, func(scope *wakeMutationScope) error {
				return writeWakeRestartRecordAt(scope, record)
			}); err != nil {
				t.Fatal(err)
			}
			previousObserve := observeAuthoritativeWakeOwner
			observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
				if got == requester {
					return wakeOwnerObservation{State: wakeOwnerDead, Reason: "owner process is not running"}, nil
				}
				return previousObserve(got)
			}
			t.Cleanup(func() { observeAuthoritativeWakeOwner = previousObserve })
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newWakeRestartFixture(t)
			leave(t, fixture)
			candidate := writeWakeSelfUpgradeCandidate(t, t.TempDir(), "candidate")
			state := selfUpgradeStateForCandidate(t, candidate)
			stubWakeSelfUpgradeVersion(t, "0.57.0")

			decision, err := maintainWakeSelfUpgrade(&state, fixture.agentDir, fixture.lock)
			if err != nil || decision.Action != wakeSelfUpgradeActionPending {
				t.Fatalf("decision=%#v err=%v, want %s", decision, err, wakeSelfUpgradeActionPending)
			}
		})
	}
}

// agent-message-queue-5bu: the live refusal reached the requester only as
// "wake restart control request refused", with no reason.
func TestDarwinWakeRestartNotifyReportsTheWakeRefusalReason(t *testing.T) {
	root, agent, owner, lock, peerSession := testDarwinWakeRestartControlLock(t)
	*peerSession = owner.SessionID + 1
	record := writeDarwinWakeRestartRecordForTest(t, root, agent, owner, lock)
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	cleanup, _, _, err := startWakeControlListenerInDirWithRestart(agentDir, root, agent, lock, make(chan os.Signal, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	err = notifyWakeRestartPlatform(agentDir, inspectWakeLock(root, agent), record)
	if err == nil || !strings.Contains(err.Error(), "not owner session") {
		t.Fatalf("notify error = %v, want the wake's session refusal", err)
	}
}
