//go:build darwin || linux

package cli

import (
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
