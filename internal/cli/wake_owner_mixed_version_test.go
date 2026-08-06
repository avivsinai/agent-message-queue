//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const ownerFenceLegacyCommit = "e37067a91b4447c3ed99bf647b71e7ec9dbf3824"
const wakeStateLegacyWriterCommit = "fbc574a8d2b26b2526dfae5d9c5c87408007ac39"
const wakeStateP2aRollbackCommit = "acd9e35511f0d5f13c9ed68349929bfcf488cecf"

func TestOwnerFencePreservesClaimAgainstExactE370Binary(t *testing.T) {
	repoRootCommand := exec.Command("git", "rev-parse", "--show-toplevel")
	repoRootOutput, err := repoRootCommand.CombinedOutput()
	if err != nil {
		t.Skipf("mixed-version git history unavailable: %v", err)
	}
	repoRoot := strings.TrimSpace(string(repoRootOutput))
	if output, err := exec.Command("git", "-C", repoRoot, "cat-file", "-e", ownerFenceLegacyCommit+"^{commit}").CombinedOutput(); err != nil {
		t.Skipf("mixed-version commit %s unavailable: %v\n%s", ownerFenceLegacyCommit, err, output)
	}

	buildRoot := t.TempDir()
	sourceRoot := filepath.Join(buildRoot, "source")
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(buildRoot, "legacy.tar")
	commandOutputForOwnerFence(
		t,
		repoRoot,
		"git",
		"archive",
		"--format=tar",
		"--output="+archivePath,
		ownerFenceLegacyCommit,
	)
	commandOutputForOwnerFence(t, "", "tar", "-xf", archivePath, "-C", sourceRoot)
	legacyBinary := filepath.Join(buildRoot, "amq-e37067a")
	commandOutputForOwnerFence(t, sourceRoot, "go", "build", "-o", legacyBinary, "./cmd/amq")

	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "mixed-version-owner-injector")
	owner, err := captureAuthoritativeCurrentWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	lock, err := newWakeLock(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock.StateGeneration = lock.Generation
	lock.StateDigest = lock.TargetDigest
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, "codex", target, lock)
	})
	_ = agentDir.Close()
	if err != nil {
		t.Fatal(err)
	}
	changedTarget := target
	changedTarget.Created = "2026-08-02T00:00:00Z"
	if err := writeWakeTarget(root, "codex", changedTarget); err != nil {
		t.Fatal(err)
	}
	inspection := inspectWakeLock(root, "codex")
	selection, err := readWakeStateSelectionForInspection(root, "codex", inspection)
	var inconclusive *wakeStateBoundInconclusiveError
	if !errors.As(err, &inconclusive) || selection.TargetPresent || selection.PreparedPresent {
		t.Fatalf("new reader accepted a bound state mismatch: selection=%#v err=%v", selection, err)
	}

	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	beforeInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeInfo.Mode().Perm() != wakeOwnerLockFileMode {
		t.Fatalf("owner fence mode = %o, want %o", beforeInfo.Mode().Perm(), wakeOwnerLockFileMode)
	}
	beforeTree := snapshotWakeCheckTree(t, root)

	commands := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "acquire",
			args: []string{"wake", "--root", root, "--me", "codex", "--inject-via", injector},
			want: "unverified",
		},
		{
			name: "doctor fix",
			args: []string{"doctor", "--ops", "--fix-wake-locks"},
			want: "unverified",
		},
		{
			name: "repair",
			args: []string{"wake", "repair", "--root", root, "--me", "codex"},
			want: "refused",
		},
		{
			name: "retire",
			args: []string{"wake", "retire", "--root", root, "--me", "codex", "--inject-via", injector},
			want: "refused",
		},
	}
	for _, test := range commands {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, legacyBinary, test.args...)
			cmd.Env = ownerFenceCommandEnv(root)
			output, _ := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("legacy command timed out: %v\n%s", test.args, output)
			}
			if !strings.Contains(strings.ToLower(string(output)), test.want) {
				t.Fatalf("legacy command %v output missing %q:\n%s", test.args, test.want, output)
			}
			assertWakeCheckTreeUnchanged(t, root, beforeTree)
		})
	}
}

func TestMixedVersionOldLiveUnboundLockUsesNewReaderFallback(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "mixed-version-unbound-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"legacy"})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	lock, err := newWakeLock(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	writeWakeLockExactForTest(t, root, "codex", lock)
	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	selection, err := readWakeStateSelectionForInspection(root, "codex", inspectWakeLock(root, "codex"))
	if err != nil || !selection.TargetPresent || !sameWakeTarget(selection.Target, target) {
		t.Fatalf("unbound old-lock selection=%#v err=%v", selection, err)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("new reader rewrote the old unbound lock")
	}
}

func TestExactP2aRollbackBinaryReadsBoundThenPublishesUnbound(t *testing.T) {
	legacyBinary := buildHistoricalAMQForWakeStateTest(t, wakeStateP2aRollbackCommit)
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	boundTree := snapshotWakeCheckTree(t, fixture.root)
	output := runHistoricalWakeCheckForStateTest(t, legacyBinary, fixture.root)
	var historicalCheck wakeCheckResult
	if err := json.Unmarshal(output, &historicalCheck); err != nil ||
		historicalCheck.Agent != fixture.me ||
		!historicalCheck.OwnerBound ||
		historicalCheck.WakeMode != wakeOwnerWakeMode ||
		historicalCheck.WakeStatus != string(wakeLockUnverified) {
		t.Fatalf("rollback binary bound inspection = %#v err=%v:\n%s", historicalCheck, err, output)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, boundTree)

	if err := fixture.release(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readyPath := filepath.Join(t.TempDir(), "rollback.ready")
	command := exec.CommandContext(
		ctx,
		legacyBinary,
		"wake",
		"--root", fixture.root,
		"--me", fixture.me,
		"--inject-via", fixture.target.InjectVia,
		"--ready-file", readyPath,
	)
	command.Dir = t.TempDir()
	command.Env = ownerFenceCommandEnv(fixture.root)
	var commandOutput bytes.Buffer
	command.Stdout = &commandOutput
	command.Stderr = &commandOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	processDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		cancel()
		<-processDone
	})

	deadline := time.Now().Add(10 * time.Second)
	var inspection wakeLockInspection
	for time.Now().Before(deadline) {
		inspection = inspectWakeLock(fixture.root, fixture.me)
		if _, readyErr := os.Stat(readyPath); inspection.Exists && readyErr == nil {
			break
		}
		select {
		case <-processDone:
			t.Fatalf("rollback binary exited before readiness: %v\n%s", waitErr, commandOutput.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); !inspection.Exists || err != nil {
		t.Fatalf("rollback binary did not publish a ready wake lock: lock=%t ready_err=%v\n%s", inspection.Exists, err, commandOutput.String())
	}
	select {
	case <-processDone:
		t.Fatalf("rollback binary exited after readiness: %v\n%s", waitErr, commandOutput.String())
	default:
	}
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil || bound || inspection.Lock.StateGeneration != "" || inspection.Lock.StateDigest != "" {
		t.Fatalf("rollback publication = %#v bound=%v err=%v", inspection.Lock, bound, err)
	}
	selection, err := readWakeStateSelectionForInspection(fixture.root, fixture.me, inspection)
	if err != nil || !selection.TargetPresent ||
		selection.Target.Root != canonicalWakeRoot(fixture.root) ||
		selection.Target.Agent != fixture.me ||
		selection.Target.InjectVia != fixture.target.InjectVia ||
		selection.Target.Owner != nil {
		t.Fatalf("new reader rollback selection = %#v err=%v", selection, err)
	}
	output = runHistoricalWakeCheckForStateTest(t, legacyBinary, fixture.root)
	historicalCheck = wakeCheckResult{}
	if err := json.Unmarshal(output, &historicalCheck); err != nil ||
		historicalCheck.Agent != fixture.me ||
		historicalCheck.OwnerBound ||
		historicalCheck.LiveWake ||
		historicalCheck.WakeMode != wakeTargetInjectVia ||
		historicalCheck.WakeStatus != string(wakeLockUnverified) {
		t.Fatalf("rollback binary unbound inspection = %#v err=%v:\n%s", historicalCheck, err, output)
	}
}

func TestBoundAndUnboundWakeClaimsCoexistAcrossCurrentAndE370Readers(t *testing.T) {
	legacyBinary := buildHistoricalAMQForWakeStateTest(t, ownerFenceLegacyCommit)
	root := secureTempDirForTest(t)
	const boundAgent = "bound"
	const survivorAgent = "survivor"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{boundAgent, survivorAgent} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{boundAgent, survivorAgent},
	}, true); err != nil {
		t.Fatal(err)
	}

	injector := writeExecutableForTest(t, "mixed-version-coexistence-injector")
	owner, err := captureAuthoritativeCurrentWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	boundTarget := mustNewWakeTargetForTest(t, root, boundAgent, injector, []string{"bound"})
	boundTarget.Owner = &owner
	boundCleanup, err := acquireAuthoritativeWakeLockWithOptions(root, boundAgent, wakeLockAcquireOptions{
		target:   &boundTarget,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(boundCleanup)

	survivorTarget := mustNewWakeTargetForTest(t, root, survivorAgent, injector, []string{"legacy"})
	if err := writeWakeTarget(root, survivorAgent, survivorTarget); err != nil {
		t.Fatal(err)
	}
	survivorLock, err := newWakeLock(root, survivorAgent, wakeLockAcquireOptions{
		target:   &survivorTarget,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Model an unbound claim left behind by an exited older writer. Keeping its
	// PID distinct from the live bound claim is what makes this an attrition
	// survivor for both generations of reader.
	survivorLock.PID++
	writeWakeLockExactForTest(t, root, survivorAgent, survivorLock)
	before := snapshotWakeCheckTree(t, root)
	boundLock := inspectWakeLock(root, boundAgent).Lock
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != boundLock.PID {
			return wakeProcessInfo{PID: pid, Running: false}
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: boundLock.ProcessStart,
			BootID:     boundLock.BootID,
			Executable: "amq",
			Args:       []string{"amq", "wake"},
		}
	})
	for _, test := range []struct {
		agent      string
		target     wakeTarget
		bound      bool
		ownerBound bool
		live       bool
		status     wakeLockStatus
		wakeMode   string
		claim      wakeClaimClass
	}{
		{
			agent:      boundAgent,
			target:     boundTarget,
			bound:      true,
			ownerBound: true,
			live:       true,
			status:     wakeLockValid,
			wakeMode:   wakeOwnerWakeMode,
			claim:      wakeClaimAuthoritative,
		},
		{
			agent:      survivorAgent,
			target:     survivorTarget,
			bound:      false,
			ownerBound: false,
			live:       false,
			status:     wakeLockStale,
			wakeMode:   wakeTargetInjectVia,
			claim:      wakeClaimGeneric,
		},
	} {
		t.Run(test.agent, func(t *testing.T) {
			inspection := inspectWakeLock(root, test.agent)
			if inspection.Status != test.status || classifyPersistedWakeClaim(inspection) != test.claim {
				t.Fatalf("inspection = %#v", inspection)
			}
			bound, err := wakeLockInspectionStateBound(inspection)
			if err != nil || bound != test.bound {
				t.Fatalf("state binding bound=%v err=%v", bound, err)
			}
			selection, err := readWakeStateSelectionForInspection(root, test.agent, inspection)
			if err != nil || selection.StatePreferred != test.bound ||
				!selection.TargetPresent || !sameWakeTarget(selection.Target, test.target) {
				t.Fatalf("state selection = %#v err=%v", selection, err)
			}
			check := inspectWakeCheck(root, test.agent)
			if check.Agent != test.agent || check.LiveWake != test.live || check.WakeStatus != string(test.status) ||
				check.OwnerBound != test.ownerBound || check.WakeMode != test.wakeMode {
				t.Fatalf("wake check = %#v", check)
			}
		})
	}

	ops := runOpsChecks(root, "test", false)
	if len(ops.WakeLocks) != 2 {
		t.Fatalf("doctor wake locks = %#v", ops.WakeLocks)
	}
	for _, test := range []struct {
		agent  string
		status wakeLockStatus
	}{
		{agent: boundAgent, status: wakeLockValid},
		{agent: survivorAgent, status: wakeLockStale},
	} {
		var found *opsWakeLock
		for i := range ops.WakeLocks {
			if ops.WakeLocks[i].Agent == test.agent {
				found = &ops.WakeLocks[i]
				break
			}
		}
		if found == nil || found.Status != string(test.status) || !found.TargetPresent ||
			found.Target != wakeTargetPath(root, test.agent) {
			t.Fatalf("doctor wake lock for %s = %#v", test.agent, found)
		}
	}

	// E370 predates `wake check`; its supported read-only inspection is doctor
	// --ops. It cannot read current 0400 owner locks, and an external test
	// process is not an amq wake, so assert only those historical observations.
	legacy := runHistoricalDoctorOpsForCoexistenceTest(t, legacyBinary, root)
	for _, test := range []struct {
		agent         string
		status        string
		target        string
		targetPresent bool
	}{
		{
			agent:         boundAgent,
			status:        string(wakeLockUnverified),
			target:        wakeTargetPath(root, boundAgent),
			targetPresent: true,
		},
		{
			agent:         survivorAgent,
			status:        string(wakeLockStale),
			target:        wakeTargetPath(root, survivorAgent),
			targetPresent: true,
		},
	} {
		lock, ok := legacy[test.agent]
		if !ok || lock.Status != test.status || lock.Target != test.target || lock.TargetPresent != test.targetPresent {
			t.Fatalf("historical doctor wake lock for %s = %#v present=%v", test.agent, lock, ok)
		}
	}
	assertWakeCheckTreeUnchanged(t, root, before)
}

func TestLegacyWriterPreservesNewerWakeStateDocument(t *testing.T) {
	legacyBinary := buildHistoricalAMQForWakeStateTest(t, wakeStateLegacyWriterCommit)
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "mixed-version-state-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)
	stateRaw := []byte(`{"schema":2,"future_field":{"preserve":"exactly"}}`)
	if err := os.WriteFile(statePath, stateRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	stateInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, legacyBinary, "wake", "recover-owner", "--root", root, "--me", "codex")
	command.Env = ownerFenceCommandEnv(root)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("legacy recover-owner timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("legacy recover-owner failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(wakeTargetPath(root, "codex")); !os.IsNotExist(err) {
		t.Fatalf("legacy writer did not commit orphan target cleanup: %v", err)
	}
	assertWakeStateSnapshotUnchangedForTest(t, statePath, stateRaw, stateInfo)
	selection, err := readWakeStateSelection(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if selection.TargetPresent || selection.StatePreferred {
		t.Fatalf("new reader did not fall back after legacy-only mutation: %#v", selection)
	}
	assertWakeStateSnapshotUnchangedForTest(t, statePath, stateRaw, stateInfo)
}

func TestWakeStateDualReadMixedVersionMatrix(t *testing.T) {
	legacyBinary := buildHistoricalAMQForWakeStateTest(t, wakeStateLegacyWriterCommit)
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "mixed-version-dual-read-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"legacy"})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	fixture := wakeStateUnixFixture{root: root, agent: "codex", agentDir: agentDir}

	selection, err := readWakeStateSelectionForTest(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if selection.StatePreferred || !selection.TargetPresent || !sameWakeTarget(selection.Target, target) {
		t.Fatalf("legacy-only selection = %#v", selection)
	}
	statePath := filepath.Join(fsq.AgentBase(root, "codex"), wakeStateFileName)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("legacy-only read created state: %v", err)
	}

	legacyBefore := runHistoricalWakeCheckForStateTest(t, legacyBinary, root)
	publishWakeStateForDualReadTest(t, fixture)
	selection, err = readWakeStateSelectionForTest(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.StatePreferred || !sameWakeTarget(selection.Target, target) {
		t.Fatalf("projected selection = %#v", selection)
	}
	legacyAfter := runHistoricalWakeCheckForStateTest(t, legacyBinary, root)
	if !bytes.Equal(legacyBefore, legacyAfter) {
		t.Fatalf("historical reader changed after state projection:\nbefore=%s\nafter=%s", legacyBefore, legacyAfter)
	}
}

func runHistoricalWakeCheckForStateTest(t *testing.T, binary, root string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "wake", "check", "--root", root, "--me", "codex", "--json")
	command.Env = ownerFenceCommandEnv(root)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("historical wake check timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("historical wake check failed: %v\n%s", err, output)
	}
	return output
}

type historicalOpsWakeLock struct {
	Status        string `json:"status"`
	Agent         string `json:"agent"`
	Target        string `json:"target"`
	TargetPresent bool   `json:"target_present"`
}

func runHistoricalDoctorOpsForCoexistenceTest(t *testing.T, binary, root string) map[string]historicalOpsWakeLock {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "doctor", "--ops", "--json")
	command.Env = ownerFenceCommandEnv(root)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("historical doctor timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("historical doctor failed: %v\n%s", err, output)
	}
	var result struct {
		Ops struct {
			WakeLocks []historicalOpsWakeLock `json:"wake_locks"`
		} `json:"ops"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode historical doctor output: %v\n%s", err, output)
	}
	if len(result.Ops.WakeLocks) == 0 {
		t.Fatalf("historical doctor omitted wake locks:\n%s", output)
	}
	locks := make(map[string]historicalOpsWakeLock, len(result.Ops.WakeLocks))
	for _, lock := range result.Ops.WakeLocks {
		locks[lock.Agent] = lock
	}
	return locks
}

func buildHistoricalAMQForWakeStateTest(t *testing.T, commit string) string {
	t.Helper()
	repoRootOutput, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		t.Skipf("mixed-version git history unavailable: %v", err)
	}
	repoRoot := strings.TrimSpace(string(repoRootOutput))
	if output, err := exec.Command("git", "-C", repoRoot, "cat-file", "-e", commit+"^{commit}").CombinedOutput(); err != nil {
		t.Skipf("mixed-version commit %s unavailable: %v\n%s", commit, err, output)
	}
	buildRoot := t.TempDir()
	sourceRoot := filepath.Join(buildRoot, "source")
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(buildRoot, "legacy.tar")
	commandOutputForOwnerFence(t, repoRoot, "git", "archive", "--format=tar", "--output="+archivePath, commit)
	commandOutputForOwnerFence(t, "", "tar", "-xf", archivePath, "-C", sourceRoot)
	binary := filepath.Join(buildRoot, "amq-legacy-state-writer")
	commandOutputForOwnerFence(t, sourceRoot, "go", "build", "-o", binary, "./cmd/amq")
	return binary
}

func commandOutputForOwnerFence(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s %v timed out", name, args)
	}
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func ownerFenceCommandEnv(root string) []string {
	env := os.Environ()
	for _, name := range []string{
		envWakeOwner,
		envRoot,
		envRootID,
		envBaseRoot,
		envBaseRootID,
		envSession,
	} {
		env = unsetEnvVar(env, name)
	}
	env = setEnvVar(env, envRoot, root)
	return setEnvVar(env, "AMQ_NO_UPDATE_CHECK", "1")
}
