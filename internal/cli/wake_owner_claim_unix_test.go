//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type ownerTransitionTreeEntry struct {
	Mode os.FileMode
	Data string
}

func seedOwnerTransitionSentinels(t *testing.T, root string) {
	t.Helper()
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "root.sentinel"), "root")
	for _, handle := range []string{"codex", "claude", "unrelated"} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(fsq.AgentInboxNew(root, handle), "preserve.md"), "mailbox:"+handle)
	}
	write(filepath.Join(fsq.AgentBase(root, "claude"), ".wake.lock"), "sibling-wake-lock")
	write(filepath.Join(fsq.AgentBase(root, "unrelated"), ".wake.target"), "unrelated-wake-target")
}

func snapshotOwnerTransitionTree(t *testing.T, root string) map[string]ownerTransitionTreeEntry {
	t.Helper()
	snapshot := make(map[string]ownerTransitionTreeEntry)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := ownerTransitionTreeEntry{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.Data = string(data)
		}
		snapshot[filepath.ToSlash(rel)] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertOwnerTransitionTreeChangesOnly(
	t *testing.T,
	before, after map[string]ownerTransitionTreeEntry,
	allowed ...string,
) map[string]bool {
	t.Helper()
	allowedSet := make(map[string]bool, len(allowed))
	for _, path := range allowed {
		allowedSet[filepath.ToSlash(path)] = true
	}
	changed := make(map[string]bool)
	for path, beforeEntry := range before {
		afterEntry, exists := after[path]
		if !exists || !reflect.DeepEqual(beforeEntry, afterEntry) {
			changed[path] = true
		}
	}
	for path := range after {
		if _, existed := before[path]; !existed {
			changed[path] = true
		}
	}
	for path := range changed {
		if !allowedSet[path] {
			t.Fatalf("owner transition changed unrelated path %q", path)
		}
	}
	return changed
}

func setupOwnerClaimTest(t *testing.T) (string, string) {
	t.Helper()
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	return root, writeExecutableForTest(t, "injector")
}

func ownerClaimTarget(t *testing.T, root, injector string, owner wakeOwner) wakeTarget {
	t.Helper()
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	return target
}

func ownerClaimProcess(pid int, start, boot string) wakeProcessInfo {
	return wakeProcessInfo{
		PID:        pid,
		Running:    true,
		StartToken: start,
		BootID:     boot,
		Executable: "/usr/local/bin/amq",
		Args:       []string{"amq", "wake", "--me", "codex"},
	}
}

func TestOwnerBoundClaimOwnerLivenessMatrix(t *testing.T) {
	ownerA := wakeOwner{PID: 4101, ProcessStart: "owner-a", BootID: "boot-1"}
	ownerB := wakeOwner{PID: 4102, ProcessStart: "owner-b", BootID: "boot-1"}
	reusedOld := wakeOwner{PID: 4301, ProcessStart: "old-start", BootID: "boot-1"}
	reusedNew := wakeOwner{PID: 4301, ProcessStart: "new-start", BootID: "boot-1"}
	tests := []struct {
		name          string
		persisted     wakeOwner
		requested     wakeOwner
		live          map[int]wakeOwner
		wantOwned     bool
		wantErrorText []string
	}{
		{
			name:      "refuses live different owner",
			persisted: ownerA,
			requested: ownerB,
			live:      map[int]wakeOwner{ownerA.PID: ownerA, ownerB.PID: ownerB},
			wantOwned: true,
		},
		{
			name:      "reclaims dead different owner",
			persisted: ownerA,
			requested: ownerB,
			live:      map[int]wakeOwner{ownerB.PID: ownerB},
		},
		{
			name:      "repairs same live owner",
			persisted: ownerA,
			requested: ownerA,
			live:      map[int]wakeOwner{ownerA.PID: ownerA},
		},
		{
			name:          "rejects replayed dead owner",
			persisted:     ownerA,
			requested:     ownerA,
			wantErrorText: []string{"requested wake owner", "not live"},
		},
		{
			name:      "treats reused PID as dead persisted owner",
			persisted: reusedOld,
			requested: reusedNew,
			live:      map[int]wakeOwner{reusedNew.PID: reusedNew},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, injector := setupOwnerClaimTest(t)
			persisted := ownerClaimTarget(t, root, injector, tt.persisted)
			if err := writeWakeTarget(root, "codex", persisted); err != nil {
				t.Fatal(err)
			}
			self := os.Getpid()
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if owner, ok := tt.live[pid]; ok {
					return ownerClaimProcess(pid, owner.ProcessStart, owner.BootID)
				}
				if pid == self {
					return ownerClaimProcess(pid, "wake-self", "boot-1")
				}
				return wakeProcessInfo{PID: pid}
			})

			requested := ownerClaimTarget(t, root, injector, tt.requested)
			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target: &requested, wakeMode: wakeTargetInjectVia,
			})
			if cleanup != nil {
				defer cleanup()
			}

			if tt.wantOwned {
				var owned *wakeOwnerAlreadyOwnedError
				if !errors.As(err, &owned) {
					t.Fatalf("acquire error = %v, want visible already-owned error", err)
				}
			} else if len(tt.wantErrorText) != 0 {
				if err == nil {
					t.Fatal("acquire succeeded, want refusal")
				}
				for _, text := range tt.wantErrorText {
					if !strings.Contains(err.Error(), text) {
						t.Fatalf("acquire error = %v, want %q", err, text)
					}
				}
			} else if err != nil {
				t.Fatalf("acquire: %v", err)
			}

			got, exists, readErr := readWakeTarget(root, "codex")
			if readErr != nil || !exists {
				t.Fatalf("read persisted target: target=%#v exists=%v err=%v", got, exists, readErr)
			}
			wantOwner := &tt.requested
			if err != nil {
				wantOwner = &tt.persisted
				if _, statErr := os.Stat(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")); !os.IsNotExist(statErr) {
					t.Fatalf("refused claim created or obscured wake lock: %v", statErr)
				}
			}
			if !sameWakeOwner(got.Owner, wantOwner) {
				t.Fatalf("persisted owner = %#v, want %#v", got.Owner, wantOwner)
			}
		})
	}
}

func TestOwnerBoundClaimFailsClosedOnUnknownOrLegacyOwner(t *testing.T) {
	tests := []struct {
		name      string
		owner     *wakeOwner
		inspect   func(int) wakeProcessInfo
		wantError string
	}{
		{
			name:  "inspection unknown",
			owner: &wakeOwner{PID: 4401, ProcessStart: "owner-a", BootID: "boot-1"},
			inspect: func(pid int) wakeProcessInfo {
				return wakeProcessInfo{PID: pid, Running: true, BootID: "boot-1"}
			},
			wantError: "unverified",
		},
		{
			name:      "legacy owner missing",
			owner:     nil,
			inspect:   func(pid int) wakeProcessInfo { return wakeProcessInfo{PID: pid} },
			wantError: "legacy",
		},
		{
			name:  "legacy owner incomplete",
			owner: &wakeOwner{PID: 4402},
			inspect: func(pid int) wakeProcessInfo {
				return wakeProcessInfo{PID: pid, Running: true}
			},
			wantError: "legacy or incomplete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, injector := setupOwnerClaimTest(t)
			persisted := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
			persisted.Owner = tt.owner
			if err := writeWakeTarget(root, "codex", persisted); err != nil {
				t.Fatal(err)
			}
			requestedOwner := wakeOwner{
				PID: 4499, ProcessStart: "requester", BootID: "boot-1",
			}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == requestedOwner.PID {
					return ownerClaimProcess(pid, requestedOwner.ProcessStart, requestedOwner.BootID)
				}
				return tt.inspect(pid)
			})
			requested := ownerClaimTarget(t, root, injector, requestedOwner)

			_, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target: &requested, wakeMode: wakeTargetInjectVia,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("acquire error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestOwnerBoundClaimFailsClosedOnCorruptTarget(t *testing.T) {
	root, injector := setupOwnerClaimTest(t)
	targetPath := wakeTargetPath(root, "codex")
	corrupt := []byte("{not-json")
	if err := os.WriteFile(targetPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	requestedOwner := wakeOwner{PID: 4451, ProcessStart: "requester", BootID: "boot-1"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == requestedOwner.PID {
			return ownerClaimProcess(pid, requestedOwner.ProcessStart, requestedOwner.BootID)
		}
		return wakeProcessInfo{PID: pid}
	})
	requested := ownerClaimTarget(t, root, injector, requestedOwner)

	_, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target: &requested, wakeMode: wakeTargetInjectVia,
	})
	if err == nil || !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("acquire error = %v, want corrupt-target refusal", err)
	}
	after, readErr := os.ReadFile(targetPath)
	if readErr != nil || string(after) != string(corrupt) {
		t.Fatalf("corrupt target was changed: err=%v data=%q", readErr, after)
	}
}

func TestOwnerBoundClaimPreservesStaleLockWhenTargetOwnerIsLive(t *testing.T) {
	root, injector := setupOwnerClaimTest(t)
	persistedOwner := wakeOwner{PID: 4501, ProcessStart: "owner-a", BootID: "boot-1"}
	persisted := ownerClaimTarget(t, root, injector, persistedOwner)
	if err := writeWakeTarget(root, "codex", persisted); err != nil {
		t.Fatal(err)
	}
	const staleWakePID = 4599
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:          staleWakePID,
		TTY:          "unknown",
		ProcessStart: "old-wake",
		BootID:       "boot-1",
		Generation:   "stale-generation",
	}, persisted))
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case persistedOwner.PID:
			return ownerClaimProcess(pid, persistedOwner.ProcessStart, persistedOwner.BootID)
		case 4502:
			return ownerClaimProcess(pid, "owner-b", "boot-1")
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	requested := ownerClaimTarget(t, root, injector, wakeOwner{
		PID: 4502, ProcessStart: "owner-b", BootID: "boot-1",
	})

	_, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target: &requested, wakeMode: wakeTargetInjectVia,
	})
	var owned *wakeOwnerAlreadyOwnedError
	if !errors.As(err, &owned) {
		t.Fatalf("acquire error = %v, want already owned", err)
	}
	after, readErr := os.ReadFile(lockPath)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("stale lock changed before ownership gate: err=%v", readErr)
	}
}

func TestOwnerBoundClaimRefusesStaleLockTargetBindingMismatch(t *testing.T) {
	root, injector := setupOwnerClaimTest(t)
	targetA := ownerClaimTarget(t, root, injector, wakeOwner{
		PID: 4551, ProcessStart: "owner-a", BootID: "boot-1",
	})
	if err := writeWakeTarget(root, "codex", targetA); err != nil {
		t.Fatal(err)
	}
	lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:          4559,
		TTY:          "unknown",
		ProcessStart: "old-wake",
		BootID:       "boot-1",
		Generation:   "stale-generation",
	}, targetA))

	targetB := ownerClaimTarget(t, root, injector, wakeOwner{
		PID: 4552, ProcessStart: "owner-b-dead", BootID: "boot-1",
	})
	targetB.InjectArgs = []string{"exec", "replacement"}
	if err := writeWakeTarget(root, "codex", targetB); err != nil {
		t.Fatal(err)
	}
	seedOwnerTransitionSentinels(t, root)
	targetPath := wakeTargetPath(root, "codex")
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	treeBefore := snapshotOwnerTransitionTree(t, root)
	targetBefore, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	requestedOwner := wakeOwner{PID: 4553, ProcessStart: "requester", BootID: "boot-1"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == requestedOwner.PID {
			return ownerClaimProcess(pid, requestedOwner.ProcessStart, requestedOwner.BootID)
		}
		return wakeProcessInfo{PID: pid}
	})
	requested := ownerClaimTarget(t, root, injector, requestedOwner)

	_, err = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target: &requested, wakeMode: wakeTargetInjectVia,
	})
	if err == nil || !strings.Contains(err.Error(), "not bound to the existing wake lock") {
		t.Fatalf("acquire error = %v, want target-binding refusal", err)
	}
	lockAfter, lockErr := os.ReadFile(lockPath)
	targetAfter, targetErr := os.ReadFile(targetPath)
	if lockErr != nil || targetErr != nil ||
		string(lockAfter) != string(lockBefore) || string(targetAfter) != string(targetBefore) {
		t.Fatalf("binding mismatch mutated state: lockErr=%v targetErr=%v", lockErr, targetErr)
	}
	treeAfter := snapshotOwnerTransitionTree(t, root)
	if changed := assertOwnerTransitionTreeChangesOnly(t, treeBefore, treeAfter); len(changed) != 0 {
		t.Fatalf("refused transition changed paths: %#v", changed)
	}
}

func TestOwnerBoundClaimSuccessfulReclaimPreservesUnrelatedTree(t *testing.T) {
	root, injector := setupOwnerClaimTest(t)
	persistedOwner := wakeOwner{PID: 4571, ProcessStart: "dead-owner", BootID: "boot-1"}
	persisted := ownerClaimTarget(t, root, injector, persistedOwner)
	if err := writeWakeTarget(root, "codex", persisted); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
		PID:          4579,
		TTY:          "unknown",
		ProcessStart: "old-wake",
		BootID:       "boot-1",
		Generation:   "stale-generation",
	}, persisted))
	seedOwnerTransitionSentinels(t, root)
	before := snapshotOwnerTransitionTree(t, root)

	requestedOwner := wakeOwner{PID: 4572, ProcessStart: "requester", BootID: "boot-1"}
	self := os.Getpid()
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case requestedOwner.PID:
			return ownerClaimProcess(pid, requestedOwner.ProcessStart, requestedOwner.BootID)
		case self:
			return ownerClaimProcess(pid, "wake-self", "boot-1")
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	requested := ownerClaimTarget(t, root, injector, requestedOwner)
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target: &requested, wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatalf("successful reclaim: %v", err)
	}
	defer cleanup()

	after := snapshotOwnerTransitionTree(t, root)
	changed := assertOwnerTransitionTreeChangesOnly(t, before, after,
		"agents/codex/.wake.lock",
		"agents/codex/.wake.target",
	)
	for _, path := range []string{"agents/codex/.wake.lock", "agents/codex/.wake.target"} {
		if !changed[path] {
			t.Fatalf("successful reclaim did not change intended metadata %q: %#v", path, changed)
		}
	}
}

func TestAcceptExistingValidConvergesDeadOwnerWithoutMaskingModeMismatch(t *testing.T) {
	tests := []struct {
		name            string
		wakeMode        string
		wantReplacement bool
		wantError       string
	}{
		{name: "matching mode replaces live orphan", wakeMode: wakeTargetInjectVia, wantReplacement: true},
		{name: "mode mismatch refuses before replacement", wakeMode: wakeInjectModeNone, wantError: "inject-mode none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, injector := setupOwnerClaimTest(t)
			const oldWakePID = 4581
			persistedOwner := wakeOwner{PID: 4582, ProcessStart: "dead-owner", BootID: "boot-1"}
			requestedOwner := wakeOwner{PID: 4583, ProcessStart: "requester", BootID: "boot-1"}
			persisted := ownerClaimTarget(t, root, injector, persistedOwner)
			requested := ownerClaimTarget(t, root, injector, requestedOwner)
			if err := writeWakeTarget(root, "codex", persisted); err != nil {
				t.Fatal(err)
			}
			lockPath := writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
				PID:          oldWakePID,
				TTY:          "unknown",
				ProcessStart: "old-wake",
				BootID:       "boot-1",
				Executable:   "/opt/homebrew/bin/amq",
				Generation:   "old-generation",
			}, persisted))
			before, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}

			self := os.Getpid()
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				switch pid {
				case oldWakePID:
					return wakeProcessInfo{
						PID: pid, Running: true, StartToken: "old-wake", BootID: "boot-1",
						Executable: "/opt/homebrew/bin/amq",
						Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
					}
				case requestedOwner.PID:
					return ownerClaimProcess(pid, requestedOwner.ProcessStart, requestedOwner.BootID)
				case self:
					return wakeProcessInfo{
						PID: pid, Running: true, StartToken: "new-wake", BootID: "boot-1",
						Executable: "/opt/homebrew/bin/amq",
						Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
					}
				default:
					return wakeProcessInfo{PID: pid}
				}
			})

			oldReplace := replaceExistingWakeLock
			replacementCalls := 0
			replaceExistingWakeLock = func(inspection wakeLockInspection) (bool, error) {
				replacementCalls++
				if inspection.PID != oldWakePID {
					return false, fmt.Errorf("unexpected replacement pid %d", inspection.PID)
				}
				if err := os.Remove(inspection.LockPath); err != nil {
					return false, err
				}
				return true, nil
			}
			t.Cleanup(func() { replaceExistingWakeLock = oldReplace })

			cleanup, acquireErr := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				acceptExistingValid: true,
				target:              &requested,
				wakeMode:            tt.wakeMode,
			})
			if cleanup != nil {
				defer cleanup()
			}
			if tt.wantError != "" {
				if acquireErr == nil || !strings.Contains(acquireErr.Error(), tt.wantError) {
					t.Fatalf("acquire error = %v, want %q", acquireErr, tt.wantError)
				}
				if replacementCalls != 0 {
					t.Fatalf("mode mismatch replaced wake %d times", replacementCalls)
				}
				after, readErr := os.ReadFile(lockPath)
				if readErr != nil || string(after) != string(before) {
					t.Fatalf("mode mismatch changed existing lock: err=%v", readErr)
				}
				return
			}
			if acquireErr != nil {
				t.Fatalf("converge live orphan: %v", acquireErr)
			}
			if replacementCalls != 1 {
				t.Fatalf("replacement calls = %d, want 1", replacementCalls)
			}
			got, exists, readErr := readWakeTarget(root, "codex")
			if readErr != nil || !exists || !sameWakeOwner(got.Owner, requested.Owner) {
				t.Fatalf("replacement target = %#v exists=%v err=%v", got, exists, readErr)
			}
		})
	}
}

func TestOwnerClaimFailsClosedWhenLockHasNoTarget(t *testing.T) {
	tests := []struct {
		name    string
		options func(t *testing.T, root, injector string) wakeLockAcquireOptions
	}{
		{
			name: "raw ownerless wake",
			options: func(_ *testing.T, _, _ string) wakeLockAcquireOptions {
				return wakeLockAcquireOptions{wakeMode: wakeInjectModeNone}
			},
		},
		{
			name: "ownerless inject-via target",
			options: func(t *testing.T, root, injector string) wakeLockAcquireOptions {
				target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
				return wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia}
			},
		},
		{
			name: "owner-bound inject-via target",
			options: func(t *testing.T, root, injector string) wakeLockAcquireOptions {
				target := ownerClaimTarget(t, root, injector, wakeOwner{
					PID: 4602, ProcessStart: "owner-b", BootID: "boot-1",
				})
				return wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, injector := setupOwnerClaimTest(t)
			writeWakeLockForTest(t, root, "codex", wakeLock{
				PID:          4611,
				TTY:          "unknown",
				ProcessStart: "old-wake",
				BootID:       "boot-1",
				Generation:   "stale-generation",
			})
			seedOwnerTransitionSentinels(t, root)
			if err := withWakeLifecycleGuard(root, "codex", func() error { return nil }); err != nil {
				t.Fatalf("seed lifecycle guard: %v", err)
			}
			before := snapshotOwnerTransitionTree(t, root)
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == 4602 {
					return ownerClaimProcess(pid, "owner-b", "boot-1")
				}
				return wakeProcessInfo{PID: pid}
			})

			_, err := acquireWakeLockWithOptions(root, "codex", tt.options(t, root, injector))
			if err == nil || !strings.Contains(err.Error(), "missing while a wake lock exists") {
				t.Fatalf("acquire error = %v, want missing-target refusal", err)
			}

			after := snapshotOwnerTransitionTree(t, root)
			if changed := assertOwnerTransitionTreeChangesOnly(t, before, after); len(changed) != 0 {
				t.Fatalf("refused claim changed tree: %#v", changed)
			}
		})
	}
}

func TestOwnerClaimRequiresRequestedOwnerForPersistedState(t *testing.T) {
	tests := []struct {
		name    string
		options func(t *testing.T, root, injector string) wakeLockAcquireOptions
	}{
		{
			name: "inject-via target omits owner",
			options: func(t *testing.T, root, injector string) wakeLockAcquireOptions {
				target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
				return wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia}
			},
		},
		{
			name: "raw ownerless wake",
			options: func(_ *testing.T, _, _ string) wakeLockAcquireOptions {
				return wakeLockAcquireOptions{wakeMode: wakeInjectModeNone}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, injector := setupOwnerClaimTest(t)
			persistedOwner := wakeOwner{PID: 4651, ProcessStart: "owner-a", BootID: "boot-1"}
			persisted := ownerClaimTarget(t, root, injector, persistedOwner)
			if err := writeWakeTarget(root, "codex", persisted); err != nil {
				t.Fatal(err)
			}

			_, err := acquireWakeLockWithOptions(root, "codex", tt.options(t, root, injector))
			if err == nil || !strings.Contains(err.Error(), "requested wake ownership") {
				t.Fatalf("acquire error = %v, want missing requested-owner refusal", err)
			}
			got, exists, readErr := readWakeTarget(root, "codex")
			if readErr != nil || !exists || !sameWakeOwner(got.Owner, persisted.Owner) {
				t.Fatalf("refused claim changed target: target=%#v exists=%v err=%v", got, exists, readErr)
			}
		})
	}
}

func TestConcurrentOwnerBoundClaimsHaveOneWinner(t *testing.T) {
	root, injector := setupOwnerClaimTest(t)
	owners := []wakeOwner{
		{PID: 4701, ProcessStart: "owner-a", BootID: "boot-1"},
		{PID: 4702, ProcessStart: "owner-b", BootID: "boot-1"},
	}
	self := os.Getpid()
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case owners[0].PID:
			return ownerClaimProcess(pid, owners[0].ProcessStart, owners[0].BootID)
		case owners[1].PID:
			return ownerClaimProcess(pid, owners[1].ProcessStart, owners[1].BootID)
		case self:
			return ownerClaimProcess(pid, "wake-self", "boot-1")
		default:
			return wakeProcessInfo{PID: pid}
		}
	})

	type result struct {
		owner   wakeOwner
		cleanup func()
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, len(owners))
	for _, owner := range owners {
		owner := owner
		go func() {
			<-start
			target := ownerClaimTarget(t, root, injector, owner)
			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target: &target, wakeMode: wakeTargetInjectVia,
			})
			results <- result{owner: owner, cleanup: cleanup, err: err}
		}()
	}
	close(start)

	first := <-results
	second := <-results
	var winner, loser result
	switch {
	case first.err == nil && second.err != nil:
		winner, loser = first, second
	case second.err == nil && first.err != nil:
		winner, loser = second, first
	default:
		t.Fatalf("claims did not produce one winner: first=%v second=%v", first.err, second.err)
	}
	defer winner.cleanup()
	var owned *wakeOwnerAlreadyOwnedError
	if !errors.As(loser.err, &owned) {
		t.Fatalf("loser error = %v, want already owned", loser.err)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists || !sameWakeOwner(persisted.Owner, &winner.owner) {
		t.Fatalf("persisted winner=%#v exists=%v err=%v, want %#v", persisted.Owner, exists, err, winner.owner)
	}
}

func TestConcurrentDifferentHandleOwnerClaimsBothSucceed(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "injector")
	owners := map[string]wakeOwner{
		"claude": {PID: 4801, ProcessStart: "claude-owner", BootID: "boot-1"},
		"codex":  {PID: 4802, ProcessStart: "codex-owner", BootID: "boot-1"},
	}
	targets := make(map[string]wakeTarget, len(owners))
	for handle, owner := range owners {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
		target := mustNewWakeTargetForTest(t, root, handle, injector, []string{"exec", handle})
		target.Owner = &owner
		targets[handle] = target
	}
	self := os.Getpid()
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case owners["claude"].PID:
			owner := owners["claude"]
			return ownerClaimProcess(pid, owner.ProcessStart, owner.BootID)
		case owners["codex"].PID:
			owner := owners["codex"]
			return ownerClaimProcess(pid, owner.ProcessStart, owner.BootID)
		case self:
			return ownerClaimProcess(pid, "wake-self", "boot-1")
		default:
			return wakeProcessInfo{PID: pid}
		}
	})

	codexGuardEntered := make(chan struct{})
	releaseCodexGuard := make(chan struct{})
	codexGuardDone := make(chan error, 1)
	go func() {
		codexGuardDone <- withWakeLifecycleGuard(root, "codex", func() error {
			close(codexGuardEntered)
			<-releaseCodexGuard
			return nil
		})
	}()
	<-codexGuardEntered

	type claimResult struct {
		cleanup func()
		err     error
	}
	claudeResult := make(chan claimResult, 1)
	go func() {
		target := targets["claude"]
		cleanup, err := acquireWakeLockWithOptions(root, "claude", wakeLockAcquireOptions{
			target: &target, wakeMode: wakeTargetInjectVia,
		})
		claudeResult <- claimResult{cleanup: cleanup, err: err}
	}()

	var claudeClaim claimResult
	select {
	case claudeClaim = <-claudeResult:
		// The Claude claim entered and completed while Codex still held its
		// lifecycle guard, proving the guard is per handle rather than per root.
	case <-time.After(2 * time.Second):
		close(releaseCodexGuard)
		<-codexGuardDone
		t.Fatal("claude claim could not progress while codex lifecycle guard was held")
	}
	if claudeClaim.err != nil {
		close(releaseCodexGuard)
		<-codexGuardDone
		t.Fatalf("claude claim failed: %v", claudeClaim.err)
	}
	defer claudeClaim.cleanup()

	close(releaseCodexGuard)
	if err := <-codexGuardDone; err != nil {
		t.Fatalf("release codex lifecycle guard: %v", err)
	}
	codexTarget := targets["codex"]
	codexCleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target: &codexTarget, wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatalf("codex claim after guard release: %v", err)
	}
	defer codexCleanup()

	for handle, wantOwner := range owners {
		persisted, exists, err := readWakeTarget(root, handle)
		if err != nil || !exists || !sameWakeOwner(persisted.Owner, &wantOwner) {
			t.Fatalf("%s target=%#v exists=%v err=%v, want only %#v", handle, persisted.Owner, exists, err, wantOwner)
		}
	}
}
