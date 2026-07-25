//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeRepairFloorRoundTripPreservesExactFileIdentity(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	messagePath := filepath.Join(fsq.AgentInboxNew(root, "codex"), "startup.md")
	if err := os.WriteFile(messagePath, []byte("startup"), 0o600); err != nil {
		t.Fatalf("write startup message: %v", err)
	}
	existing, err := snapshotWakeExistingMessages(root, "codex")
	if err != nil {
		t.Fatalf("snapshot existing messages: %v", err)
	}
	injector := writeExecutableForTest(t, "repair-floor-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	lock := bindWakeLockToTarget(wakeLock{
		Root:       canonicalWakeRoot(root),
		Agent:      "codex",
		Generation: "generation-one",
		BootID:     wakeRepairTestBootID,
	}, target)

	floor, err := newWakeRepairFloor(root, "codex", lock, target, existing)
	if err != nil {
		t.Fatalf("newWakeRepairFloor: %v", err)
	}
	if err := writeWakeRepairFloor(root, "codex", floor); err != nil {
		t.Fatalf("writeWakeRepairFloor: %v", err)
	}
	got, exists, err := readWakeRepairFloor(root, "codex")
	if err != nil || !exists {
		t.Fatalf("readWakeRepairFloor: exists=%v err=%v", exists, err)
	}
	if err := validateWakeRepairFloor(got, root, "codex", lock, target); err != nil {
		t.Fatalf("validateWakeRepairFloor: %v", err)
	}
	if len(got.Existing) != 1 || got.Existing["startup.md"] != existing["startup.md"] {
		t.Fatalf("persisted identities = %#v, want %#v", got.Existing, existing)
	}
	info, err := os.Stat(wakeRepairFloorPath(root, "codex"))
	if err != nil {
		t.Fatalf("stat wake repair floor: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("wake repair floor mode = %o, want 0600", got)
	}
}

func TestWakeRepairFloorRejectsAuthorityMismatch(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "repair-floor-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	lock := bindWakeLockToTarget(wakeLock{
		Root:       canonicalWakeRoot(root),
		Agent:      "codex",
		Generation: "generation-one",
		BootID:     wakeRepairTestBootID,
	}, target)
	floor, err := newWakeRepairFloor(root, "codex", lock, target, nil)
	if err != nil {
		t.Fatalf("newWakeRepairFloor: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*wakeRepairFloor, *wakeLock, *wakeTarget)
		want   string
	}{
		{
			name:   "generation",
			mutate: func(floor *wakeRepairFloor, _ *wakeLock, _ *wakeTarget) { floor.Generation = "generation-two" },
			want:   "generation mismatch",
		},
		{
			name:   "target",
			mutate: func(_ *wakeRepairFloor, _ *wakeLock, target *wakeTarget) { target.InjectArgs = []string{"other"} },
			want:   "target mismatch",
		},
		{
			name: "boot",
			mutate: func(_ *wakeRepairFloor, lock *wakeLock, _ *wakeTarget) {
				lock.BootID = "22222222-2222-2222-2222-222222222222"
			},
			want: "boot identity mismatch",
		},
		{
			name:   "root identity",
			mutate: func(floor *wakeRepairFloor, _ *wakeLock, _ *wakeTarget) { floor.RootIdentity = "invalid" },
			want:   "root identity mismatch",
		},
		{
			name: "owner",
			mutate: func(floor *wakeRepairFloor, _ *wakeLock, _ *wakeTarget) {
				floor.Owner = &wakeOwner{PID: 42}
			},
			want: "owner mismatch",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changedFloor := floor
			changedFloor.Existing = cloneWakeFileIdentities(floor.Existing)
			changedLock := lock
			changedTarget := target
			tc.mutate(&changedFloor, &changedLock, &changedTarget)
			err := validateWakeRepairFloor(changedFloor, root, "codex", changedLock, changedTarget)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadWakeRepairFloorRejectsSymlink(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	target := filepath.Join(secureTempDirForTest(t), "floor")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, wakeRepairFloorPath(root, "codex")); err != nil {
		t.Fatalf("symlink floor: %v", err)
	}

	_, exists, err := readWakeRepairFloor(root, "codex")
	if !exists {
		t.Fatal("symlink floor should be reported present")
	}
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("error = %v, want symlink refusal", err)
	}
}

func TestFreshWakeWinsRepairGapWithoutReusingDeadFloor(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "repair-floor-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	sourceLock := bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Root:       canonicalWakeRoot(root),
		Agent:      "codex",
		Generation: "dead-generation",
		BootID:     wakeRepairTestBootID,
	}, target)
	writeWakeLockForTest(t, root, "codex", sourceLock)
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("writeWakeTarget: %v", err)
	}
	sourceFloor := writeWakeRepairFloorForTest(t, root, "codex", target, map[string]wakeFileIdentity{
		"startup.md": {Device: 1, Inode: 2, CTimeSec: 3, CTimeNsec: 4},
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == os.Getpid() {
			return wakeProcessInfo{
				PID:        pid,
				Running:    true,
				StartToken: "self-start",
				BootID:     wakeRepairTestBootID,
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
			}
		}
		return wakeProcessInfo{PID: pid, Running: false}
	})
	if err := os.Remove(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")); err != nil {
		t.Fatalf("remove dead lock: %v", err)
	}
	digest, err := wakeRepairFloorDigest(sourceFloor)
	if err != nil {
		t.Fatalf("wakeRepairFloorDigest: %v", err)
	}
	lineage := &wakeRepairLineage{
		source: wakeRepairSource{
			Root:               sourceFloor.Root,
			RootIdentity:       sourceFloor.RootIdentity,
			Agent:              sourceFloor.Agent,
			DeadGeneration:     sourceFloor.Generation,
			BootID:             sourceFloor.BootID,
			Owner:              sourceFloor.Owner,
			SourceTargetDigest: sourceFloor.TargetDigest,
			SourceFloorDigest:  digest,
		},
		floor: sourceFloor,
	}

	freshCleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatalf("fresh wake acquisition: %v", err)
	}
	defer freshCleanup()
	if _, exists, err := readWakeRepairFloor(root, "codex"); err != nil || exists {
		t.Fatalf("fresh acquisition must clear dead floor: exists=%v err=%v", exists, err)
	}

	repairCleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:        &target,
		wakeMode:      wakeTargetInjectVia,
		repairLineage: lineage,
	})
	if repairCleanup != nil {
		repairCleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "changed before repair acquisition") {
		t.Fatalf("repair acquisition error = %v, want takeover refusal", err)
	}
}

func TestAcquireWakeLockRejectsOwnerBearingRepairLineageBeforeAuthoritativeRouting(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "repair-floor-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &wakeOwner{PID: 4242}

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:        &target,
		wakeMode:      wakeTargetInjectVia,
		repairLineage: &wakeRepairLineage{},
	})
	if cleanup != nil {
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "owner-bearing wake state requires") {
		t.Fatalf("acquireWakeLockWithOptions error = %v, want owner-bearing repair refusal", err)
	}
}

func TestValidateWakeRepairLineageGuardedRejectsMissingOrChangedFloor(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, root string, floor wakeRepairFloor)
		wantReason string
	}{
		{
			name: "floor disappeared",
			mutate: func(t *testing.T, root string, _ wakeRepairFloor) {
				t.Helper()
				if err := os.Remove(wakeRepairFloorPath(root, "codex")); err != nil {
					t.Fatalf("remove repair floor: %v", err)
				}
			},
			wantReason: "disappeared before acquisition",
		},
		{
			name: "floor digest changed",
			mutate: func(t *testing.T, root string, floor wakeRepairFloor) {
				t.Helper()
				floor.Existing["changed.md"] = wakeFileIdentity{Device: 5, Inode: 6, CTimeSec: 7, CTimeNsec: 8}
				if err := writeWakeRepairFloor(root, "codex", floor); err != nil {
					t.Fatalf("write changed repair floor: %v", err)
				}
			},
			wantReason: "changed before acquisition",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			injector := writeExecutableForTest(t, "repair-floor-injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
			writeWakeLockForTest(t, root, "codex", bindWakeLockToTarget(wakeLock{
				PID:        4242,
				Generation: "dead-generation",
				BootID:     wakeRepairTestBootID,
			}, target))
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatalf("writeWakeTarget: %v", err)
			}
			floor := writeWakeRepairFloorForTest(t, root, "codex", target, map[string]wakeFileIdentity{
				"startup.md": {Device: 1, Inode: 2, CTimeSec: 3, CTimeNsec: 4},
			})
			digest, err := wakeRepairFloorDigest(floor)
			if err != nil {
				t.Fatalf("wakeRepairFloorDigest: %v", err)
			}
			lineage := &wakeRepairLineage{
				source: wakeRepairSource{
					Root:               floor.Root,
					RootIdentity:       floor.RootIdentity,
					Agent:              floor.Agent,
					DeadGeneration:     floor.Generation,
					BootID:             floor.BootID,
					Owner:              floor.Owner,
					SourceTargetDigest: floor.TargetDigest,
					SourceFloorDigest:  digest,
				},
				floor: floor,
			}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == os.Getpid() {
					return wakeProcessInfo{PID: pid, Running: true, BootID: wakeRepairTestBootID}
				}
				return wakeProcessInfo{PID: pid, Running: false}
			})

			tc.mutate(t, root, floor)
			err = validateWakeRepairLineageGuarded(root, "codex", target, lineage)
			if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("validateWakeRepairLineageGuarded error = %v, want %q", err, tc.wantReason)
			}
		})
	}
}

func TestWakeCleanupRemovesOnlyItsRepairFloorGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	injector := writeExecutableForTest(t, "repair-floor-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "self-start",
			BootID:     wakeRepairTestBootID,
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	cleanup, err := acquireWakeLock(root, "codex", &target)
	if err != nil {
		t.Fatalf("acquireWakeLock: %v", err)
	}
	current := inspectWakeLock(root, "codex")
	floor, err := newWakeRepairFloor(root, "codex", current.Lock, target, nil)
	if err != nil {
		t.Fatalf("newWakeRepairFloor: %v", err)
	}
	if err := writeWakeRepairFloor(root, "codex", floor); err != nil {
		t.Fatalf("writeWakeRepairFloor: %v", err)
	}
	if err := removeWakeRepairFloorIfGenerationGuarded(root, "codex", "replacement-generation"); err != nil {
		t.Fatalf("remove replacement generation: %v", err)
	}
	if _, exists, err := readWakeRepairFloor(root, "codex"); err != nil || !exists {
		t.Fatalf("wrong generation removed floor: exists=%v err=%v", exists, err)
	}

	cleanup()
	if _, err := os.Stat(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")); !os.IsNotExist(err) {
		t.Fatalf("wake lock survived cleanup: %v", err)
	}
	if _, err := os.Stat(wakeRepairFloorPath(root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("wake repair floor survived cleanup: %v", err)
	}
}

func TestRepairWakeNormalCleanupPreservesFloorReplacedBeforeCleanup(t *testing.T) {
	tests := []struct {
		name   string
		mutate bool
	}{
		{name: "byte-identical new inode"},
		{name: "same generation and source digest changed bytes", mutate: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}
			injector := writeExecutableForTest(t, "repair-cleanup-injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
			sourceLock := bindWakeLockToTarget(wakeLock{
				PID:        4242,
				Root:       canonicalWakeRoot(root),
				Agent:      "codex",
				Generation: "dead-generation",
				BootID:     wakeRepairTestBootID,
			}, target)
			writeWakeLockForTest(t, root, "codex", sourceLock)
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatalf("writeWakeTarget: %v", err)
			}
			sourceFloor := writeWakeRepairFloorForTest(t, root, "codex", target, nil)
			sourceFloorDigest, err := wakeRepairFloorDigest(sourceFloor)
			if err != nil {
				t.Fatalf("digest source floor: %v", err)
			}
			lineage := &wakeRepairLineage{
				source: wakeRepairSource{
					Root:               sourceFloor.Root,
					RootIdentity:       sourceFloor.RootIdentity,
					Agent:              sourceFloor.Agent,
					DeadGeneration:     sourceFloor.Generation,
					BootID:             sourceFloor.BootID,
					SourceTargetDigest: sourceFloor.TargetDigest,
					SourceFloorDigest:  sourceFloorDigest,
				},
				floor: sourceFloor,
			}
			if err := os.Remove(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")); err != nil {
				t.Fatalf("remove dead wake lock: %v", err)
			}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				if pid == os.Getpid() {
					return wakeProcessInfo{
						PID:        pid,
						Running:    true,
						StartToken: "self-start",
						BootID:     wakeRepairTestBootID,
						Executable: "/opt/homebrew/bin/amq",
						Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
					}
				}
				return wakeProcessInfo{PID: pid, Running: false}
			})

			var authority wakeRepairFloorAuthority
			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target:               &target,
				wakeMode:             wakeTargetInjectVia,
				repairLineage:        lineage,
				repairFloorAuthority: &authority,
			})
			if err != nil {
				t.Fatalf("acquire repair wake lock: %v", err)
			}
			current := inspectWakeLock(root, "codex")
			childFloor, err := newInheritedWakeRepairFloor(
				lineage.source,
				current.Lock,
				target,
				sourceFloor.Existing,
			)
			if err != nil {
				t.Fatalf("new inherited child floor: %v", err)
			}
			agentDir, err := openWakeAgentDir(root, "codex")
			if err != nil {
				t.Fatalf("open child agent directory: %v", err)
			}
			err = agentDir.withFD(func(dirfd int) error {
				var captureErr error
				authority, captureErr = writeWakeRepairFloorAndCaptureAuthorityAt(
					dirfd,
					agentDir,
					root,
					childFloor,
				)
				return captureErr
			})
			_ = agentDir.Close()
			if err != nil {
				t.Fatalf("publish child floor: %v", err)
			}
			replacement := replaceWakeRepairFloorWithNewInodeForTest(
				t,
				root,
				"codex",
				tc.mutate,
			)

			cleanup()

			if _, err := os.Lstat(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")); !os.IsNotExist(err) {
				t.Fatalf("repair child lock survived normal cleanup: %v", err)
			}
			got, err := os.ReadFile(wakeRepairFloorPath(root, "codex"))
			if err != nil {
				t.Fatalf("replacement wake repair floor was removed: %v", err)
			}
			if !bytes.Equal(got, replacement) {
				t.Fatalf("replacement wake repair floor changed during cleanup")
			}
		})
	}
}
