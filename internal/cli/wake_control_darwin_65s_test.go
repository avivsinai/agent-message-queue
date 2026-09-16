//go:build darwin

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDarwinControlBindRefusesDetachedDirectoryOnSwap (65s) proves the
// validate-bind window is closed: a directory swap after validation but before
// bind causes the listener to REFUSE, not bind a socket in the detached OLD
// directory that canonical clients cannot reach.
//
// Before the fix, withWakeLifecycleGuardInDir validated the canonical directory
// + generation + removed stale sockets, then RETURNED (released the guard).
// listenDarwinUnixAt bound and secureDarwinControlSocketAt chmoded OUTSIDE the
// guard. A directory swap in that gap bound the listener to the detached OLD
// directory; canonical clients could not reach it and the listener still
// reported success. SILENT NON-DELIVERY.
//
// The fix moves bind + secure UNDER the lifecycle guard and revalidates the
// canonical identity (validateWakeStateAgentDirAt: SameFile against the
// retained dirfd) immediately before bind. A swap renames the old directory
// away and replaces it at the canonical path, so SameFile fails and we refuse.
//
// Mutation RED: revert to binding OUTSIDE the guard (remove the revalidation
// and the in-guard bind) -> the swap goes undetected, the listener binds the
// detached directory, and start returns nil instead of an error.
func TestDarwinControlBindRefusesDetachedDirectoryOnSwap(t *testing.T) {
	root := secureTempDirForTest(t)
	agent := "codex"
	injector := writeExecutableForTest(t, "darwin-control-swap-injector")
	owner := wakeOwner{
		PID:          os.Getpid(),
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
	defer func() { _ = agentDir.Close() }()
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return publishAuthoritativeWakeClaimAt(scope, root, agent, target, lock)
	}); err != nil {
		t.Fatal(err)
	}

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
		return wakeOwnerObservation{State: wakeOwnerSame, Reason: "test owner evidence"}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })
	stubWakeProcessSID(t, func(pid int) (int, error) { return owner.SessionID, nil })

	// The swap hook: between the first validation and the pre-bind
	// revalidation, rename the canonical agent directory away and replace it
	// with a fresh directory at the same path. The retained dirfd still points
	// at the OLD (now-detached) inode; validateWakeStateAgentDirAt opens the
	// canonical PATH, stats it, and SameFile fails -> REFUSE.
	agentPath := filepath.Join(root, "agents", agent)
	hooks := &darwinWakeControlTestHooks{
		beforeBindRevalidate: func(_ *wakeAgentDir) {
			detached := agentPath + ".detached-65s"
			if err := os.Rename(agentPath, detached); err != nil {
				t.Fatalf("swap: rename old away: %v", err)
			}
			if err := os.MkdirAll(agentPath, 0o700); err != nil {
				t.Fatalf("swap: mkdir replacement: %v", err)
			}
		},
	}

	_, _, _, err = startWakeControlListenerInDirOwnedWithRestart(
		agentDir, root, agent, lock, false, hooks, nil,
	)
	if err == nil {
		t.Fatal("control listener started despite directory swap (65s: must REFUSE rather than bind a detached directory)")
	}
}
