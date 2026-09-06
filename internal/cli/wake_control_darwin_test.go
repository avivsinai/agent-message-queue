//go:build darwin

package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

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
