//go:build darwin

package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDarwinStableOwnerStopRefusesBoundInconclusiveBeforeControl(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	if err := os.Remove(filepath.Join(fixture.agentDir.path, wakeStateFileName)); err != nil {
		t.Fatal(err)
	}

	err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		capability, err := prepareAuthoritativeWakeStopPlatform(scope, fixture.inspection)
		if err == nil && capability.stop != nil {
			t.Fatal("bound-inconclusive stop created a cooperative control capability")
		}
		return err
	})
	var inconclusive *wakeStateBoundInconclusiveError
	if !errors.As(err, &inconclusive) {
		t.Fatalf("stable-stop error = %v, want bound inconclusive", err)
	}
}

func testDarwinControlLock(t *testing.T) (string, string, wakeLock) {
	t.Helper()
	root := secureTempDirForTest(t)
	const agent = "codex"
	lock := wakeLock{PID: os.Getpid(), WakeMode: wakeTargetInjectVia, Generation: "0123456789abcdef0123456789abcdef"}
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	writeWakeLockForTest(t, root, agent, lock)
	return root, agent, lock
}

func testDarwinOwnerControlLock(
	t *testing.T,
) (string, string, wakeOwner, wakeLock, *wakeOwnerIdentityState, *int) {
	t.Helper()
	root := secureTempDirForTest(t)
	const agent = "codex"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "darwin-owner-control-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, agent, injector, nil)
	target.Owner = &owner
	lock := bindWakeLockToTarget(wakeLock{
		PID:          os.Getpid(),
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-23T00:00:00Z",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent, "--inject-via", injector},
		Generation:   "0123456789abcdef0123456789abcdef",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return publishAuthoritativeWakeClaimAt(scope, root, agent, target, lock)
	}); err != nil {
		_ = agentDir.Close()
		t.Fatal(err)
	}
	_ = agentDir.Close()

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != lock.PID {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: lock.ProcessStart,
			BootID:     lock.BootID,
			Executable: lock.Executable,
			Args:       lock.Args,
		}
	})
	ownerState := wakeOwnerSame
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if got != owner {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		return wakeOwnerObservation{State: ownerState, Reason: "test owner evidence"}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
	peerSession := owner.SessionID
	stubWakeProcessSID(t, func(pid int) (int, error) {
		if pid != os.Getpid() {
			t.Fatalf("peer session pid = %d, want %d", pid, os.Getpid())
		}
		return peerSession, nil
	})
	return root, agent, owner, lock, &ownerState, &peerSession
}

func testDarwinWakeRestartControlLock(
	t *testing.T,
) (string, string, wakeOwner, wakeLock, *int) {
	t.Helper()
	root := secureTempDirForTest(t)
	const agent = "codex"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	lock := wakeLock{
		PID:          os.Getpid(),
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-08-07T00:00:00Z",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		WakeMode:     wakeInjectModeNone,
		Generation:   "0123456789abcdef0123456789abcdef",
		ResumeSchema: wakeResumeSchemaV2,
		ResumeOwner:  &owner,
	}
	configureWakeRestartAdvertisementPlatform(&lock, root, agent)
	writeWakeLockForTest(t, root, agent, lock)

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != lock.PID {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: lock.ProcessStart,
			BootID:     lock.BootID,
			Executable: lock.Executable,
			Args:       lock.Args,
		}
	})
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if got != owner {
			t.Fatalf("observed restart owner = %#v, want %#v", got, owner)
		}
		return wakeOwnerObservation{State: wakeOwnerSame, Reason: "test owner evidence"}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
	peerSession := owner.SessionID
	stubWakeProcessSID(t, func(pid int) (int, error) {
		if pid != os.Getpid() {
			t.Fatalf("restart peer session pid = %d, want %d", pid, os.Getpid())
		}
		return peerSession, nil
	})
	return root, agent, owner, lock, &peerSession
}

func writeDarwinWakeRestartRecordForTest(
	t *testing.T,
	root string,
	agent string,
	owner wakeOwner,
	lock wakeLock,
) wakeRestartRecord {
	t.Helper()
	record := wakeRestartRecord{
		Schema:     wakeRestartSchemaV1,
		RequestID:  "fedcba9876543210fedcba9876543210",
		Status:     wakeRestartPending,
		Root:       canonicalWakeRoot(root),
		Agent:      agent,
		Generation: lock.Generation,
		Owner:      owner,
		Candidate:  validWakeImageEvidenceForTest(),
	}
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, record)
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func readDarwinWakeRestartRecordForTest(
	t *testing.T,
	root string,
	agent string,
) (wakeRestartRecord, bool) {
	t.Helper()
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	var record wakeRestartRecord
	var exists bool
	if err := agentDir.withFD(func(dirfd int) error {
		var readErr error
		record, exists, readErr = readWakeRestartRecordAt(dirfd, agentDir)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	return record, exists
}

func TestDarwinStableOwnerStopTreatsDifferentLivePIDOccupantAsAbsent(t *testing.T) {
	root, agent, _, lock, _, _ := testDarwinOwnerControlLock(t)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != lock.PID {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "78901",
			BootID:     lock.BootID,
			Executable: lock.Executable,
			Args:       lock.Args,
		}
	})

	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	err = withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		expected := inspectWakeLockAt(dirfd, agentDir, root, agent)
		if expected.Status != wakeLockStale {
			t.Fatalf("reused wake PID status = %s, want stale", expected.Status)
		}
		capability, err := prepareAuthoritativeWakeStopPlatform(scope, expected)
		if err != nil {
			return err
		}
		defer func() { _ = capability.Close() }()
		if !capability.Absent {
			t.Fatal("different live PID occupant was treated as the recorded wake")
		}
		return capability.Stop(wakeOwnerReleaseAuthorization{})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDarwinAuthoritativeStopRefusesDetachAfterPreparation(t *testing.T) {
	root, agent, owner, _, _, _ := testDarwinOwnerControlLock(t)
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	var capability authoritativeWakeStopCapability
	err = withExistingWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		expected := inspectWakeLockAt(dirfd, agentDir, root, agent)
		var prepareErr error
		capability, prepareErr = prepareAuthoritativeWakeStopPlatform(scope, expected)
		return prepareErr
	})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{".wake.lock", wakeTargetFileName, wakeStateFileName, wakeLifecycleGuardFileName}
	_ = detachGenericWakeAgentDirForTest(t, agentDir.path, names...)
	successorBefore := snapshotDetachedWakeFiles(t, agentDir.path, names...)

	err = capability.Stop(wakeOwnerReleaseAuthorization{Token: &owner, Rollback: true})
	if err == nil || !strings.Contains(err.Error(), "canonical wake agent directory") {
		t.Fatalf("authoritative stop error = %v, want detached-authority refusal", err)
	}
	assertDetachedWakeFilesUnchanged(t, agentDir.path, successorBefore)
}

func sendDarwinOwnerControlRequest(
	t *testing.T,
	root string,
	agent string,
	lock wakeLock,
	request wakeControlOwnerRequest,
) string {
	t.Helper()
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	name, err := darwinControlSocketName(agentDir, lock.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialDarwinUnixAt(agentDir, name, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line)
}

func TestDarwinOwnerControlRejectsNamedOperationsAndContinues(t *testing.T) {
	root, agent, owner, lock, _, _ := testDarwinOwnerControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	for _, operation := range []string{"prompt_observed", "unknown_op", "release"} {
		response := sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
			Generation: lock.Generation,
			Owner:      &owner,
			Operation:  operation,
		})
		if response != "" {
			t.Fatalf("named operation %q response = %q, want refusal", operation, response)
		}
	}
	select {
	case <-stopped:
		t.Fatal("named operation requested owner shutdown")
	default:
	}
	if current := inspectWakeLock(root, agent); !current.Exists ||
		current.Lock.Generation != lock.Generation {
		t.Fatalf("named operation changed owner claim: %#v", current)
	}

	result := make(chan string, 1)
	go func() {
		result <- sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
			Generation: lock.Generation,
			Owner:      &owner,
		})
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("valid release after named-operation refusals did not quiesce notification work")
	}
	if !inspectWakeLock(root, agent).Exists {
		t.Fatal("valid release removed owner claim before notification work quiesced")
	}
	markStopped()
	select {
	case got := <-result:
		if got != "ACK" {
			t.Fatalf("valid release after named-operation refusals = %q, want ACK", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("valid release after named-operation refusals did not acknowledge")
	}
	if inspectWakeLock(root, agent).Exists {
		t.Fatal("valid release after named-operation refusals left the lock")
	}
}

func TestDarwinWakeRestartControlEnqueuesSignalWithoutStopping(t *testing.T) {
	root, agent, owner, lock, _ := testDarwinWakeRestartControlLock(t)
	record := writeDarwinWakeRestartRecordForTest(t, root, agent, owner, lock)
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	restartSignals := make(chan os.Signal, 1)
	cleanup, stopped, _, err := startWakeControlListenerInDirWithRestart(
		agentDir,
		root,
		agent,
		lock,
		restartSignals,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	expected := inspectWakeLock(root, agent)
	if err := notifyWakeRestartPlatform(agentDir, expected, record); err != nil {
		t.Fatal(err)
	}
	select {
	case signal := <-restartSignals:
		if signal != syscall.SIGUSR1 {
			t.Fatalf("restart signal = %v, want SIGUSR1", signal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart control did not enqueue SIGUSR1")
	}
	select {
	case signal := <-restartSignals:
		t.Fatalf("restart control enqueued duplicate signal %v", signal)
	default:
	}
	select {
	case <-stopped:
		t.Fatal("restart control requested owner shutdown")
	default:
	}
	if current := inspectWakeLock(root, agent); !current.Exists ||
		current.Lock.Generation != lock.Generation {
		t.Fatalf("restart control changed owner claim: %#v", current)
	}
	observed, exists := readDarwinWakeRestartRecordForTest(t, root, agent)
	if !exists || !sameWakeRestartRecord(record, observed) {
		t.Fatalf("restart control changed pending record: exists=%v record=%#v", exists, observed)
	}
}

func TestDarwinWakeRestartControlRefusesMismatchedAuthorization(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(
			t *testing.T,
			root string,
			agent string,
			owner wakeOwner,
			lock wakeLock,
			record wakeRestartRecord,
			request *wakeControlOwnerRequest,
			peerSession *int,
		)
	}{
		{
			name: "request id",
			mutate: func(_ *testing.T, _, _ string, _ wakeOwner, _ wakeLock, _ wakeRestartRecord, request *wakeControlOwnerRequest, _ *int) {
				request.RequestID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
		},
		{
			name: "generation",
			mutate: func(_ *testing.T, _, _ string, _ wakeOwner, _ wakeLock, _ wakeRestartRecord, request *wakeControlOwnerRequest, _ *int) {
				request.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
		},
		{
			name: "owner",
			mutate: func(_ *testing.T, _, _ string, owner wakeOwner, _ wakeLock, _ wakeRestartRecord, request *wakeControlOwnerRequest, _ *int) {
				owner.PID++
				request.Owner = &owner
			},
		},
		{
			name: "session",
			mutate: func(_ *testing.T, _, _ string, owner wakeOwner, _ wakeLock, _ wakeRestartRecord, _ *wakeControlOwnerRequest, peerSession *int) {
				*peerSession = owner.SessionID + 1
			},
		},
		{
			name: "missing record",
			mutate: func(t *testing.T, root, agent string, _ wakeOwner, _ wakeLock, _ wakeRestartRecord, _ *wakeControlOwnerRequest, _ *int) {
				agentDir, err := openWakeAgentDir(root, agent)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = agentDir.Close() }()
				if err := withWakeLifecycleGuardInDir(agentDir, func(_ int) error {
					return os.Remove(filepath.Join(agentDir.path, wakeRestartFileName))
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, agent, owner, lock, peerSession := testDarwinWakeRestartControlLock(t)
			record := writeDarwinWakeRestartRecordForTest(t, root, agent, owner, lock)
			restartSignals := make(chan os.Signal, 1)
			agentDir, err := openWakeAgentDir(root, agent)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = agentDir.Close() }()
			cleanup, stopped, _, err := startWakeControlListenerInDirWithRestart(
				agentDir,
				root,
				agent,
				lock,
				restartSignals,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			request := wakeControlOwnerRequest{
				Generation: record.Generation,
				RequestID:  record.RequestID,
				Owner:      &owner,
				Operation:  wakeControlRestartOperation,
			}
			test.mutate(t, root, agent, owner, lock, record, &request, peerSession)
			if response := sendDarwinOwnerControlRequest(t, root, agent, lock, request); response != "" {
				t.Fatalf("restart refusal response = %q, want closed connection", response)
			}
			select {
			case signal := <-restartSignals:
				t.Fatalf("refused restart enqueued signal %v", signal)
			default:
			}
			select {
			case <-stopped:
				t.Fatal("refused restart requested owner shutdown")
			default:
			}
			if current := inspectWakeLock(root, agent); !current.Exists ||
				current.Lock.Generation != lock.Generation {
				t.Fatalf("refused restart changed owner claim: %#v", current)
			}
		})
	}
}

func TestDarwinWakeRestartControlRefusesWrongUID(t *testing.T) {
	root, agent, owner, lock, _ := testDarwinWakeRestartControlLock(t)
	record := writeDarwinWakeRestartRecordForTest(t, root, agent, owner, lock)
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return authorizeDarwinWakeRestartControlAt(
			dirfd,
			agentDir,
			root,
			agent,
			lock,
			wakeControlOwnerRequest{
				Generation: record.Generation,
				RequestID:  record.RequestID,
				Owner:      &owner,
				Operation:  wakeControlRestartOperation,
			},
			os.Getpid(),
			uint32(os.Geteuid()+1),
		)
	})
	if err == nil || !strings.Contains(err.Error(), "uid") {
		t.Fatalf("wrong-uid restart error = %v, want uid refusal", err)
	}
}

func TestDarwinWakeRestartControlRefusesDeadOwner(t *testing.T) {
	root, agent, owner, lock, _ := testDarwinWakeRestartControlLock(t)
	record := writeDarwinWakeRestartRecordForTest(t, root, agent, owner, lock)
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if got != owner {
			t.Fatalf("observed restart owner = %#v, want %#v", got, owner)
		}
		return deadWakeOwnerObservation("test owner exited"), nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return authorizeDarwinWakeRestartControlAt(
			dirfd,
			agentDir,
			root,
			agent,
			lock,
			wakeControlOwnerRequest{
				Generation: record.Generation,
				RequestID:  record.RequestID,
				Owner:      &owner,
				Operation:  wakeControlRestartOperation,
			},
			os.Getpid(),
			uint32(os.Geteuid()),
		)
	})
	if err == nil || !strings.Contains(err.Error(), "dead") {
		t.Fatalf("dead-owner restart error = %v, want dead refusal", err)
	}
}

func TestDarwinWakeRestartControlRefusesWithoutRestartChannel(t *testing.T) {
	root, agent, owner, lock, _ := testDarwinWakeRestartControlLock(t)
	record := writeDarwinWakeRestartRecordForTest(t, root, agent, owner, lock)
	cleanup, stopped, _, err := startWakeControlListener(root, agent, lock)
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
	if response != "" {
		t.Fatalf("restart without channel response = %q, want refusal", response)
	}
	select {
	case <-stopped:
		t.Fatal("restart without channel requested owner shutdown")
	default:
	}
	if current := inspectWakeLock(root, agent); !current.Exists ||
		current.Lock.Generation != lock.Generation {
		t.Fatalf("restart without channel changed owner claim: %#v", current)
	}
}

func TestDarwinWakeRestartControlRefusesEmptyStopOperation(t *testing.T) {
	root, agent, owner, lock, _ := testDarwinWakeRestartControlLock(t)
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	restartSignals := make(chan os.Signal, 1)
	cleanup, stopped, _, err := startWakeControlListenerInDirWithRestart(
		agentDir,
		root,
		agent,
		lock,
		restartSignals,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	response := sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
		Generation: lock.Generation,
		Owner:      &owner,
	})
	if response != "" {
		t.Fatalf("ordinary empty-operation response = %q, want refusal", response)
	}
	select {
	case <-stopped:
		t.Fatal("ordinary empty operation requested wake shutdown")
	default:
	}
	select {
	case signal := <-restartSignals:
		t.Fatalf("ordinary empty operation enqueued signal %v", signal)
	default:
	}
	if current := inspectWakeLock(root, agent); !current.Exists ||
		current.Lock.Generation != lock.Generation {
		t.Fatalf("ordinary empty operation changed wake lock: %#v", current)
	}
}

func TestDarwinCooperativeStopACKAfterLockRemoval(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	go func() { <-stopped; markStopped() }()
	inspection := inspectWakeLock(root, agent)
	replaced, err := cooperativeStopInjectVia(inspection)
	if err != nil || !replaced {
		t.Fatalf("stop=(%v,%v)", replaced, err)
	}
	if current := inspectWakeLock(root, agent); current.Exists {
		t.Fatal("ACK arrived before lock removal")
	}
}

func TestDarwinRetireTargetChangeAfterQuiesceRefusesClaimRemoval(t *testing.T) {
	root := secureTempDirForTest(t)
	const agent = "codex"
	establishDoctorWakeLifecycleGuardForTest(t, root, agent)
	injector := writeExecutableForTest(t, "darwin-retire-target")
	requested := mustNewWakeTargetForTest(t, root, agent, injector, []string{"terminal-a"})
	if err := writeWakeTarget(root, agent, requested); err != nil {
		t.Fatal(err)
	}
	lock := bindWakeLockToTarget(wakeLock{
		PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: agent,
		WakeMode: wakeTargetInjectVia, Generation: "darwin-target-consent-generation",
	}, requested)
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	lockPath := writeWakeLockForTest(t, root, agent, lock)
	listenerDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listenerDir.Close() }()
	replacement := mustNewWakeTargetForTest(t, root, agent, injector, []string{"terminal-b"})
	hooks := &darwinWakeControlTestHooks{afterLoopStopped: func() {
		if err := writeWakeTarget(root, agent, replacement); err != nil {
			t.Error(err)
		}
	}}
	cleanup, stopped, markStopped, err := startWakeControlListenerInDirOwned(
		listenerDir, root, agent, lock, false, hooks,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	clientDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientDir.Close() }()
	type result struct {
		stopped bool
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		ok, stopErr := cooperativeStopInjectViaInDir(clientDir, inspectWakeLock(root, agent), &requested)
		resultCh <- result{stopped: ok, err: stopErr}
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not accept target-bound stop")
	}
	markStopped()
	select {
	case got := <-resultCh:
		if got.stopped || got.err == nil {
			t.Fatalf("stop result = %+v, want refusal without ACK", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("target-changed stop did not return")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("target-changed stop removed lock: %v", err)
	}
}

func assertDarwinControlCleanupWaitsForAcceptedHandler(
	t *testing.T,
	stopped <-chan struct{},
	markStopped func(),
	afterLoopStopped <-chan struct{},
	releaseHandler chan struct{},
	cleanup func(),
	waitForHandler func(),
) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("accepted handler did not request loop stop")
	}
	markStopped()
	select {
	case <-afterLoopStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("accepted handler did not reach post-quiesce cleanup")
	}

	cleanupDone := make(chan struct{})
	go func() {
		cleanup()
		close(cleanupDone)
	}()
	cleanupReturnedEarly := false
	select {
	case <-cleanupDone:
		cleanupReturnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseHandler)
	waitForHandler()
	select {
	case <-cleanupDone:
	case <-time.After(2 * time.Second):
		t.Fatal("control cleanup did not finish after accepted handler")
	}
	if cleanupReturnedEarly {
		t.Fatal("control cleanup returned before accepted handler completed")
	}
}

func TestDarwinControlCleanupWaitsForAcceptedInjectViaHandler(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	afterLoopStopped := make(chan struct{})
	releaseHandler := make(chan struct{})
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, stopped, markStopped, err := startWakeControlListenerInDirOwned(
		agentDir,
		root,
		agent,
		lock,
		true,
		&darwinWakeControlTestHooks{
			afterLoopStopped: func() {
				close(afterLoopStopped)
				<-releaseHandler
			},
		},
	)
	if err != nil {
		_ = agentDir.Close()
		t.Fatal(err)
	}
	defer cleanup()

	type stopResult struct {
		stopped bool
		err     error
	}
	resultCh := make(chan stopResult, 1)
	go func() {
		stopped, err := cooperativeStopInjectVia(inspectWakeLock(root, agent))
		resultCh <- stopResult{stopped: stopped, err: err}
	}()
	assertDarwinControlCleanupWaitsForAcceptedHandler(
		t,
		stopped,
		markStopped,
		afterLoopStopped,
		releaseHandler,
		cleanup,
		func() {
			select {
			case got := <-resultCh:
				if got.err != nil || !got.stopped {
					t.Fatalf("stop=(%v,%v)", got.stopped, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cooperative stop did not finish")
			}
		},
	)
	if current := inspectWakeLock(root, agent); current.Exists {
		t.Fatal("ACK arrived before lock removal")
	}
}

func TestDarwinCooperativeStopACKWaitsForLoopExit(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	type result struct {
		stopped bool
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		stopped, err := cooperativeStopInjectVia(inspectWakeLock(root, agent))
		resultCh <- result{stopped: stopped, err: err}
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not request loop stop")
	}
	if current := inspectWakeLock(root, agent); !current.Exists || current.Lock.Generation != lock.Generation {
		t.Fatal("listener unpublished its generation before loop exit")
	}
	if err := withWakeLifecycleGuard(root, agent, func() error { return nil }); err != nil {
		t.Fatalf("listener held lifecycle guard while waiting for loop exit: %v", err)
	}
	select {
	case got := <-resultCh:
		t.Fatalf("stop returned before loop exit: %+v", got)
	default:
	}
	markStopped()
	select {
	case got := <-resultCh:
		if got.err != nil || !got.stopped {
			t.Fatalf("stop=(%v,%v)", got.stopped, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not ACK after loop exit")
	}
}

func TestDarwinCooperativeStopKeepsGenerationPublishedUntilLoopExit(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	type result struct {
		stopped bool
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		stopped, err := cooperativeStopInjectVia(inspectWakeLock(root, agent))
		resultCh <- result{stopped: stopped, err: err}
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not request loop stop")
	}
	defer markStopped()

	current := inspectWakeLock(root, agent)
	if !current.Exists || current.Lock.Generation != lock.Generation {
		t.Fatalf("stopping generation was unpublished before loop exit: %#v", current)
	}
	concurrentCleanup, acquireErr := acquireWakeLock(root, agent, nil)
	if concurrentCleanup != nil {
		concurrentCleanup()
	}
	if acquireErr == nil {
		t.Fatalf("concurrent acquire started a second generation before the old loop exited; stopping=%#v", current)
	}
	if !strings.Contains(acquireErr.Error(), "already running") {
		t.Fatalf("concurrent acquire error = %v, want already-running refusal", acquireErr)
	}

	markStopped()
	select {
	case got := <-resultCh:
		if got.err != nil || !got.stopped {
			t.Fatalf("stop=(%v,%v)", got.stopped, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not complete after loop exit")
	}
}

func TestDarwinCooperativeStopCompletionOutlivesAuthenticationDeadline(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	type result struct {
		stopped bool
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		stopped, err := cooperativeStopInjectVia(inspectWakeLock(root, agent))
		resultCh <- result{stopped: stopped, err: err}
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not request loop stop")
	}

	// Authentication is complete, but the synchronous injector may still be
	// running for longer than the authentication deadline.
	time.Sleep(2200 * time.Millisecond)
	markStopped()
	select {
	case got := <-resultCh:
		if got.err != nil || !got.stopped {
			t.Fatalf("stop=(%v,%v), want completion after delayed loop exit", got.stopped, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not complete after delayed loop exit")
	}
}

func TestDarwinControlTokenMismatchRefused(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	name, err := darwinControlSocketName(agentDir, lock.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialDarwinUnixAt(agentDir, name, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte("wrong-token\n"))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if strings.TrimSpace(line) == "ACK" {
		t.Fatal("mismatched token acknowledged")
	}
	if !inspectWakeLock(root, agent).Exists {
		t.Fatal("mismatched token removed lock")
	}
}

func TestDarwinOwnerControlRefusesGenerationOnlyAndWrongSessionBeforeQuiesce(t *testing.T) {
	t.Run("generation only", func(t *testing.T) {
		root, agent, _, lock, _, _ := testDarwinOwnerControlLock(t)
		cleanup, stopped, _, err := startWakeControlListener(root, agent, lock)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if got := sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
			Generation: lock.Generation,
		}); got == "ACK" {
			t.Fatal("generation-only owner request was acknowledged")
		}
		select {
		case <-stopped:
			t.Fatal("generation-only owner request quiesced notification work")
		default:
		}
		if !inspectWakeLock(root, agent).Exists {
			t.Fatal("generation-only owner request removed claim")
		}
	})

	t.Run("wrong peer session", func(t *testing.T) {
		root, agent, owner, lock, _, peerSession := testDarwinOwnerControlLock(t)
		*peerSession = owner.SessionID + 1
		cleanup, stopped, _, err := startWakeControlListener(root, agent, lock)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if got := sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
			Generation: lock.Generation,
			Owner:      &owner,
		}); got == "ACK" {
			t.Fatal("wrong-session owner request was acknowledged")
		}
		select {
		case <-stopped:
			t.Fatal("wrong-session owner request quiesced notification work")
		default:
		}
		if !inspectWakeLock(root, agent).Exists {
			t.Fatal("wrong-session owner request removed claim")
		}
	})
}

func TestDarwinOwnerControlObservationCloseFailureFailsBeforeQuiesce(t *testing.T) {
	root, agent, owner, lock, _, _ := testDarwinOwnerControlLock(t)
	monitorErr := errors.New("owner monitor failed")
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if got != owner {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		monitor := newWakeOwnerObservationMonitor(nil)
		monitor.finish(monitorErr)
		return wakeOwnerObservation{State: wakeOwnerSame, monitor: monitor}, nil
	}
	cleanup, stopped, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if got := sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
		Generation: lock.Generation,
		Owner:      &owner,
	}); got == "ACK" {
		t.Fatal("owner monitor failure was acknowledged")
	}
	select {
	case <-stopped:
		t.Fatal("owner monitor failure quiesced notification work")
	default:
	}
	if !inspectWakeLock(root, agent).Exists {
		t.Fatal("owner monitor failure removed authoritative claim")
	}
}

func TestDarwinOwnerControlAuthenticatedReleaseACKsAfterExactClaimRemoval(t *testing.T) {
	root, agent, owner, lock, _, _ := testDarwinOwnerControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	result := make(chan string, 1)
	go func() {
		result <- sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
			Generation: lock.Generation,
			Owner:      &owner,
		})
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("authenticated owner request did not quiesce notification work")
	}
	if !inspectWakeLock(root, agent).Exists {
		t.Fatal("owner claim was removed before notification work quiesced")
	}

	markStopped()
	select {
	case got := <-result:
		if got != "ACK" {
			t.Fatalf("authenticated owner release response = %q, want ACK", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authenticated owner release did not acknowledge")
	}
	if inspectWakeLock(root, agent).Exists {
		t.Fatal("authenticated owner release left the lock")
	}
	if _, exists, err := readWakeTarget(root, agent); err != nil || exists {
		t.Fatalf("authenticated owner release target exists=%v err=%v", exists, err)
	}
}

func TestDarwinOwnerControlCompletesAfterInheritedOutputInjectorTimeout(t *testing.T) {
	root, agent, owner, lock, _, _ := testDarwinOwnerControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	result := make(chan string, 1)
	go func() {
		result <- sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
			Generation: lock.Generation,
			Owner:      &owner,
		})
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("authenticated owner request did not quiesce notification work")
	}

	scriptPath := filepath.Join(secureTempDirForTest(t), "background-output.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 2 &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	injectDone := make(chan error, 1)
	start := time.Now()
	go func() {
		injectDone <- injectVia(&wakeConfig{
			injectVia:     scriptPath,
			injectTimeout: 50 * time.Millisecond,
		}, "test")
		markStopped()
	}()
	select {
	case err := <-injectDone:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("injector result = %v, want timeout", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("inherited-output injector exceeded bounded shutdown")
	}
	select {
	case got := <-result:
		if got != "ACK" {
			t.Fatalf("owner release response = %q, want ACK", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("owner release remained blocked after injector timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("owner release completed after %s", elapsed)
	}
	if inspectWakeLock(root, agent).Exists {
		t.Fatal("owner release left the lock after bounded injector timeout")
	}
}

func TestDarwinControlCleanupWaitsForAcceptedOwnerHandler(t *testing.T) {
	root, agent, owner, lock, _, _ := testDarwinOwnerControlLock(t)
	afterLoopStopped := make(chan struct{})
	releaseHandler := make(chan struct{})
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, stopped, markStopped, err := startWakeControlListenerInDirOwned(
		agentDir,
		root,
		agent,
		lock,
		true,
		&darwinWakeControlTestHooks{
			afterLoopStopped: func() {
				close(afterLoopStopped)
				<-releaseHandler
			},
		},
	)
	if err != nil {
		_ = agentDir.Close()
		t.Fatal(err)
	}
	defer cleanup()

	resultCh := make(chan string, 1)
	go func() {
		resultCh <- sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
			Generation: lock.Generation,
			Owner:      &owner,
		})
	}()
	assertDarwinControlCleanupWaitsForAcceptedHandler(
		t,
		stopped,
		markStopped,
		afterLoopStopped,
		releaseHandler,
		cleanup,
		func() {
			select {
			case response := <-resultCh:
				if response != "ACK" {
					t.Fatalf("authenticated owner release response = %q, want ACK", response)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("authenticated owner release did not finish")
			}
		},
	)
	if inspectWakeLock(root, agent).Exists {
		t.Fatal("authenticated owner release left the lock")
	}
	if _, exists, err := readWakeTarget(root, agent); err != nil || exists {
		t.Fatalf("authenticated owner release target exists=%v err=%v", exists, err)
	}
}

func TestDarwinOwnerControlReauthorizesAfterQuiesce(t *testing.T) {
	root, agent, owner, lock, ownerState, _ := testDarwinOwnerControlLock(t)
	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	result := make(chan string, 1)
	go func() {
		result <- sendDarwinOwnerControlRequest(t, root, agent, lock, wakeControlOwnerRequest{
			Generation: lock.Generation,
			Owner:      &owner,
		})
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("authorized owner request did not quiesce notification work")
	}
	if !inspectWakeLock(root, agent).Exists {
		t.Fatal("owner claim was removed before loop quiesced")
	}
	*ownerState = wakeOwnerUnknown
	markStopped()
	select {
	case got := <-result:
		if got == "ACK" {
			t.Fatal("pre-quiesce authorization survived changed post-quiesce owner evidence")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner control request did not finish after post-quiesce refusal")
	}
	if !inspectWakeLock(root, agent).Exists {
		t.Fatal("post-quiesce reauthorization failure removed owner claim")
	}
}

func TestDarwinOwnerWakeChildStopsThroughInheritedPrivateFD(t *testing.T) {
	cmd := exec.Command("sh", "-c", `eval "dd bs=1 count=1 <&$AMQ_WAKE_PRIVATE_STOP_FD >/dev/null 2>&1"`)
	cmd.Env = os.Environ()
	capability, err := configureAuthoritativeWakeChild(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = capability.Close()
		t.Fatal(err)
	}
	waiter := newWakeProcessWaiter(cmd.Process)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = waiter.waitForExit(time.Second)
	})
	if err := capability.Bind(cmd.Process); err != nil {
		_ = capability.Close()
		t.Fatal(err)
	}
	if err := capability.Stop(); err != nil {
		_ = capability.Close()
		t.Fatal(err)
	}
	if err := waiter.waitForExit(2 * time.Second); err != nil {
		_ = capability.Close()
		t.Fatalf("private-stop child did not exit: %v", err)
	}
	if err := capability.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinOwnerWakeChildAlreadyExitedStillClosesStableCapability(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.Env = os.Environ()
	capability, err := configureAuthoritativeWakeChild(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = capability.Close()
		t.Fatal(err)
	}
	waiter := newWakeProcessWaiter(cmd.Process)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = waiter.waitForExit(time.Second)
	})
	if err := capability.Bind(cmd.Process); err != nil {
		_ = capability.Close()
		t.Fatal(err)
	}
	if err := waiter.waitForExit(2 * time.Second); err != nil {
		_ = capability.Close()
		t.Fatal(err)
	}
	if err := capability.Stop(); err != nil {
		_ = capability.Close()
		t.Fatalf("already-exited child stop: %v", err)
	}
	if err := capability.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinMissingControlMetadataRefused(t *testing.T) {
	_, err := cooperativeStopInjectVia(wakeLockInspection{Lock: wakeLock{Generation: "g"}})
	if err == nil {
		t.Fatal("legacy inject-via wake accepted without control metadata")
	}
}

func TestDarwinControlRemovesStaleSocketBeforeListen(t *testing.T) {
	root, agent, lock := testDarwinControlLock(t)
	oldSocket := filepath.Join(fsq.AgentBase(root, agent), ".w.stale-generation")
	if err := os.WriteFile(oldSocket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock.ControlSocket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatalf("stale socket prevented listener: %v", err)
	}
	if _, err := os.Lstat(oldSocket); !os.IsNotExist(err) {
		t.Fatalf("old generation socket still exists: %v", err)
	}
	cleanup()
}

func TestDarwinControlStartPinsAuthorizedAgentDirectory(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "root")
	parkedRoot := filepath.Join(parent, "authorized-root")
	replacementRoot := filepath.Join(parent, "replacement-root")
	const agent = "codex"
	lock := wakeLock{PID: os.Getpid(), WakeMode: wakeTargetInjectVia, Generation: "0123456789abcdef0123456789abcdef"}
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	writeWakeLockForTest(t, root, agent, lock)
	if err := fsq.EnsureAgentDirs(replacementRoot, agent); err != nil {
		t.Fatal(err)
	}
	replacementSocket := filepath.Join(fsq.AgentBase(replacementRoot, agent), filepath.Base(lock.ControlSocket))
	const sentinel = "replacement-tree-sentinel"
	if err := os.WriteFile(replacementSocket, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	var swapOnce sync.Once
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		swapOnce.Do(func() {
			if err := os.Rename(root, parkedRoot); err != nil {
				t.Fatalf("park authorized root: %v", err)
			}
			if err := os.Rename(replacementRoot, root); err != nil {
				t.Fatalf("install replacement root: %v", err)
			}
		})
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})

	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatalf("start control listener: %v", err)
	}
	parkedSocket := filepath.Join(fsq.AgentBase(parkedRoot, agent), filepath.Base(lock.ControlSocket))
	if info, err := os.Lstat(parkedSocket); err != nil || info.Mode()&os.ModeSocket == 0 {
		cleanup()
		t.Fatalf("listener was not bound in authorized agent directory: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(fsq.AgentBase(root, agent), filepath.Base(lock.ControlSocket))); err != nil || string(got) != sentinel {
		cleanup()
		t.Fatalf("replacement tree was modified: got=%q err=%v", got, err)
	}

	cleanup()
	if _, err := os.Lstat(parkedSocket); !os.IsNotExist(err) {
		t.Fatalf("authorized socket survived cleanup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(fsq.AgentBase(root, agent), filepath.Base(lock.ControlSocket))); err != nil || string(got) != sentinel {
		t.Fatalf("cleanup modified replacement tree: got=%q err=%v", got, err)
	}
}

func TestDarwinControlCleanupPinsAuthorizedAgentDirectory(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "root")
	parkedRoot := filepath.Join(parent, "authorized-root")
	replacementRoot := filepath.Join(parent, "replacement-root")
	const agent = "codex"
	lock := wakeLock{PID: os.Getpid(), WakeMode: wakeTargetInjectVia, Generation: "0123456789abcdef0123456789abcdef"}
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	writeWakeLockForTest(t, root, agent, lock)

	cleanup, _, _, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatalf("start control listener: %v", err)
	}
	if err := fsq.EnsureAgentDirs(replacementRoot, agent); err != nil {
		cleanup()
		t.Fatal(err)
	}
	replacementSocket := filepath.Join(fsq.AgentBase(replacementRoot, agent), filepath.Base(lock.ControlSocket))
	const sentinel = "replacement-tree-sentinel"
	if err := os.WriteFile(replacementSocket, []byte(sentinel), 0o600); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := os.Rename(root, parkedRoot); err != nil {
		cleanup()
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Rename(replacementRoot, root); err != nil {
		cleanup()
		t.Fatalf("install replacement root: %v", err)
	}

	cleanup()
	parkedSocket := filepath.Join(fsq.AgentBase(parkedRoot, agent), filepath.Base(lock.ControlSocket))
	if _, err := os.Lstat(parkedSocket); !os.IsNotExist(err) {
		t.Fatalf("authorized socket survived cleanup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(fsq.AgentBase(root, agent), filepath.Base(lock.ControlSocket))); err != nil || string(got) != sentinel {
		t.Fatalf("cleanup modified replacement tree: got=%q err=%v", got, err)
	}
}

func TestDarwinControlAuthenticationPinsAuthorizedAgentDirectory(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "root")
	parkedRoot := filepath.Join(parent, "authorized-root")
	replacementRoot := filepath.Join(parent, "replacement-root")
	const agent = "codex"
	lock := wakeLock{
		PID:        os.Getpid(),
		Root:       canonicalWakeRoot(root),
		Agent:      agent,
		WakeMode:   wakeTargetInjectVia,
		Generation: "0123456789abcdef0123456789abcdef",
	}
	lock.ControlSocket = wakeControlSocketPath(root, agent, lock.Generation)
	writeWakeLockExactForTest(t, root, agent, lock)
	writeWakeLockExactForTest(t, replacementRoot, agent, lock)

	var inspections atomic.Int32
	swapErr := make(chan error, 1)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if inspections.Add(1) == 2 {
			if err := os.Rename(root, parkedRoot); err != nil {
				swapErr <- fmt.Errorf("park authorized root: %w", err)
			} else if err := os.Rename(replacementRoot, root); err != nil {
				swapErr <- fmt.Errorf("install replacement root: %w", err)
			}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})

	cleanup, stopped, markStopped, err := startWakeControlListener(root, agent, lock)
	if err != nil {
		t.Fatalf("start control listener: %v", err)
	}
	defer cleanup()
	type stopResult struct {
		stopped bool
		err     error
	}
	resultCh := make(chan stopResult, 1)
	go func() {
		stopped, err := cooperativeStopInjectVia(wakeLockInspection{Root: root, Agent: agent, Lock: lock})
		resultCh <- stopResult{stopped: stopped, err: err}
	}()
	select {
	case got := <-resultCh:
		if got.err == nil || got.stopped {
			t.Fatalf("stop=(%v,%v), want pre-stop authority refusal", got.stopped, got.err)
		}
	case err := <-swapErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("pre-stop authority refusal did not return")
	}
	select {
	case <-stopped:
		t.Fatal("detached retained directory requested stop")
	default:
	}
	markStopped()

	if _, err := os.Lstat(filepath.Join(fsq.AgentBase(parkedRoot, agent), ".wake.lock")); err != nil {
		t.Fatalf("detached authorized generation was mutated: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fsq.AgentBase(root, agent), ".wake.lock")); err != nil {
		t.Fatalf("replacement-tree generation was modified: %v", err)
	}
}
