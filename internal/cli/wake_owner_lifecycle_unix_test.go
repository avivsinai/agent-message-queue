//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func absentAuthoritativeWakeStopForTest(
	_ *wakeMutationScope,
	expected wakeLockInspection,
) (authoritativeWakeStopCapability, error) {
	return authoritativeWakeStopCapability{Inspection: expected, Absent: true}, nil
}

func TestAcquireAuthoritativeWakeClaimPublishesOwnerAndCleanupPreservesLifetime(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-acquire-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner

	ownerState := wakeOwnerSame
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if err := validateAuthoritativeWakeOwner(got); err != nil {
			t.Fatalf("observed invalid owner %#v: %v", got, err)
		}
		if ownerState == wakeOwnerSame {
			observation := liveWakeOwnerObservationForTest()
			observation.Reason = "test owner evidence"
			return observation, nil
		}
		return wakeOwnerObservation{State: ownerState, Reason: "test owner evidence"}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	wakeRunning := true
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != os.Getpid() {
			return wakeProcessInfo{PID: pid}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    wakeRunning,
			StartToken: "67890",
			BootID:     owner.BootID,
			Executable: "/usr/local/bin/amq",
			Args:       []string{"amq", "wake", "--me", "codex", "--root", root},
		}
	})

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatalf("acquire owner claim: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != wakeOwnerLockFileMode {
		t.Fatalf("owner lock mode = %o, want %o", got, wakeOwnerLockFileMode)
	}
	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockValid || inspection.Lock.OwnerSchema != wakeOwnerLockSchema ||
		inspection.Lock.WakeMode != wakeOwnerWakeMode || !sameWakeOwner(inspection.Lock.Owner, &owner) {
		t.Fatalf("owner inspection = %#v", inspection)
	}
	ownerState = wakeOwnerUnknown
	if err := writeWakeReadyFile(root, "codex", filepath.Join(root, "owner.ready"), inspection); err == nil ||
		!strings.Contains(err.Error(), "owner") {
		t.Fatalf("readiness with unknown owner error = %v, want owner refusal", err)
	}
	if current := inspectWakeLock(root, "codex"); !sameWakeLockGeneration(inspection, current) {
		t.Fatal("failed owner readiness validation changed claim")
	}
	ownerState = wakeOwnerSame
	readyPath := filepath.Join(root, "owner.ready")
	if err := writeWakeReadyFile(root, "codex", readyPath, inspection); err != nil {
		t.Fatalf("write owner readiness: %v", err)
	}
	differentReadyOwner := owner
	differentReadyOwner.PID++
	differentReadyOwner.ProcessStart = "23456"
	if _, err := validateWakeReadyFileAgainstOwner(
		root,
		"codex",
		readyPath,
		&differentReadyOwner,
	); err == nil || !strings.Contains(err.Error(), "requested owner") {
		t.Fatalf("different requested owner readiness error = %v", err)
	}
	if err := writeWakePreparedFile(root, "codex", inspection); err != nil {
		t.Fatalf("write owner prepared marker: %v", err)
	}
	preparedInfo, err := os.Lstat(wakePreparedPath(root, "codex"))
	if err != nil || preparedInfo.Mode().Perm() != 0o600 {
		t.Fatalf("owner prepared marker info=%v err=%v", preparedInfo, err)
	}

	cleanup()
	after := inspectWakeLock(root, "codex")
	if !sameWakeLockGeneration(inspection, after) {
		t.Fatalf("ordinary wake cleanup removed owner claim: before=%#v after=%#v", inspection, after)
	}

	_, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		acceptExistingValid: true,
		target:              &target,
		wakeMode:            wakeTargetInjectVia,
	})
	var alreadyRunning *wakeAlreadyRunningError
	if !errors.As(err, &alreadyRunning) || !sameWakeLockGeneration(inspection, alreadyRunning.Inspection) {
		t.Fatalf("same-owner reuse error = %v, want exact existing generation", err)
	}

	differentTarget := target
	differentOwner := owner
	differentOwner.PID++
	differentOwner.ProcessStart = "23456"
	differentTarget.Owner = &differentOwner
	_, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		acceptExistingValid: true,
		target:              &differentTarget,
		wakeMode:            wakeTargetInjectVia,
	})
	if err == nil || !strings.Contains(err.Error(), "owned by live process") {
		t.Fatalf("different-owner acquisition error = %v, want live-owner conflict", err)
	}

	wakeRunning = false
	_, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		acceptExistingValid: true,
		target:              &target,
		wakeMode:            wakeTargetInjectVia,
	})
	if err == nil || !strings.Contains(err.Error(), "unusable wake") {
		t.Fatalf("same-owner damaged wake error = %v, want recover-owner refusal", err)
	}
	preserved := inspectWakeLock(root, "codex")
	if !sameWakeLockGeneration(inspection, preserved) {
		t.Fatalf("same-owner damaged wake changed claim: before=%#v after=%#v", inspection, preserved)
	}
}

func TestConcurrentAuthoritativeAcquisitionPublishesOneGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-contention-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "67890",
			BootID:     owner.BootID,
			Executable: "/usr/local/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		}
	})
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
		return wakeOwnerObservation{State: wakeOwnerSame}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	const contenders = 16
	start := make(chan struct{})
	results := make(chan error, contenders)
	var group sync.WaitGroup
	for range contenders {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			requested := target
			_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				acceptExistingValid: true,
				target:              &requested,
				wakeMode:            wakeTargetInjectVia,
			})
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)

	winners := 0
	reusers := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		var alreadyRunning *wakeAlreadyRunningError
		if errors.As(err, &alreadyRunning) {
			reusers++
			continue
		}
		t.Fatalf("contention result: %v", err)
	}
	if winners != 1 || reusers != contenders-1 {
		t.Fatalf("contention winners=%d reusers=%d, want 1/%d", winners, reusers, contenders-1)
	}
	inspection := inspectWakeLock(root, "codex")
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	err = withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		_, err := validateAuthoritativeWakeClaimPairAt(scope, inspection)
		return err
	})
	_ = agentDir.Close()
	if err != nil {
		t.Fatalf("contended claim is incomplete: %v", err)
	}
}

func TestRecoverOwnerRequiresExactTokenAndCallerSessionForLiveOwner(t *testing.T) {
	tests := []struct {
		name        string
		ownerState  wakeOwnerIdentityState
		token       bool
		callerSID   int
		callerErr   error
		wantSuccess bool
		wantReason  string
	}{
		{
			name:        "live owner exact token and session releases",
			ownerState:  wakeOwnerSame,
			token:       true,
			callerSID:   99,
			wantSuccess: true,
		},
		{
			name:       "token replay from another session refuses",
			ownerState: wakeOwnerSame,
			token:      true,
			callerSID:  100,
			wantReason: "OS session",
		},
		{
			name:       "caller session lookup failure refuses",
			ownerState: wakeOwnerSame,
			token:      true,
			callerErr:  errors.New("session unavailable"),
			wantReason: "session unavailable",
		},
		{
			name:       "missing live owner token refuses",
			ownerState: wakeOwnerSame,
			callerSID:  99,
			wantReason: "token",
		},
		{
			name:        "dead owner needs no token",
			ownerState:  wakeOwnerDead,
			wantSuccess: true,
		},
		{
			name:       "unknown owner preserves claim",
			ownerState: wakeOwnerUnknown,
			wantReason: "unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			injector := writeExecutableForTest(t, "owner-recover-injector")
			owner := wakeOwner{
				PID:          4242,
				ProcessStart: "12345",
				BootID:       "11111111-1111-1111-1111-111111111111",
				SessionID:    99,
			}
			target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
			target.Owner = &owner
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatal(err)
			}
			lock := bindWakeLockToTarget(wakeLock{
				PID:          5151,
				TTY:          "unknown",
				ProcessStart: "67890",
				BootID:       owner.BootID,
				Generation:   "owner-recover-generation",
				OwnerSchema:  wakeOwnerLockSchema,
				Owner:        &owner,
			}, target)
			lock.WakeMode = wakeOwnerWakeMode
			lockPath := writeWakeLockForTest(t, root, "codex", lock)
			if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
				t.Fatal(err)
			}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return wakeProcessInfo{PID: pid}
			})
			oldObserve := observeAuthoritativeWakeOwner
			observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
				if got != owner {
					t.Fatalf("observed owner = %#v, want %#v", got, owner)
				}
				return wakeOwnerObservation{State: test.ownerState, Reason: test.wantReason}, nil
			}
			t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
			stubWakeProcessSID(t, func(pid int) (int, error) {
				if pid != os.Getpid() {
					t.Fatalf("caller sid lookup pid = %d, want %d", pid, os.Getpid())
				}
				return test.callerSID, test.callerErr
			})
			if test.token {
				encoded, err := encodeWakeOwnerEnv(owner)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv(envWakeOwner, encoded)
			} else {
				t.Setenv(envWakeOwner, "")
			}

			result, err := recoverOwnerWakeWithStopPreparer(
				root,
				"codex",
				absentAuthoritativeWakeStopForTest,
			)
			if test.wantSuccess {
				if err != nil || result.Status != "recovered" {
					t.Fatalf("recover result = %#v err=%v", result, err)
				}
				if inspectWakeLock(root, "codex").Exists {
					t.Fatal("successful recovery preserved owner lock")
				}
				return
			}
			if err == nil || result.Status != "refused" ||
				!strings.Contains(strings.ToLower(result.Reason), strings.ToLower(test.wantReason)) {
				t.Fatalf("recover result = %#v err=%v, want reason %q", result, err, test.wantReason)
			}
			if !inspectWakeLock(root, "codex").Exists {
				t.Fatal("refused recovery removed owner lock")
			}
		})
	}
}
