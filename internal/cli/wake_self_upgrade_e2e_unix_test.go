//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const wakeSelfUpgradePTYOwnerHelperEnv = "AMQ_TEST_WAKE_SELF_UPGRADE_PTY_OWNER"

func prepareWakeSelfUpgradeE2E(t *testing.T) (stable, oldBinary, newBinary, root, oldVersion, newVersion string) {
	t.Helper()
	temp := t.TempDir()
	oldDir := filepath.Join(temp, "releases", "old")
	newDir := filepath.Join(temp, "releases", "new")
	installDir := filepath.Join(temp, "install")
	for _, dir := range []string{oldDir, newDir, installDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldBinary = filepath.Join(oldDir, "amq")
	newBinary = filepath.Join(newDir, "amq")
	oldVersion = "0.58.0"
	newVersion = "0.59.0"
	repoRoot := wakeSelfUpgradeE2ERepoRoot(t)
	buildVersionedWakeRestartBinary(t, repoRoot, oldBinary, oldVersion)
	buildVersionedWakeRestartBinary(t, repoRoot, newBinary, newVersion)
	stable = filepath.Join(installDir, "amq")
	if err := os.Symlink(oldBinary, stable); err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(temp, "root")
	init := exec.Command(stable, "init", "--root", root, "--agents", "codex")
	init.Env = wakeABICleanEnv()
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("initialize self-upgrade E2E: %v\n%s", err, output)
	}
	// coop exec starts its wake with --baseline-existing. Pre-existing unread
	// work is therefore retained without an input-delivery debt, so this E2E
	// exercises the real quiescent self-upgrade path while proving that unread
	// work survives the replacement.
	runWakeSelfUpgradeCommand(
		t,
		oldBinary,
		"send", "--root", root, "--me", "user", "--to", "codex",
		"--subject", "before-self-upgrade", "--body", "preserve this unread message",
	)
	return stable, oldBinary, newBinary, root, oldVersion, newVersion
}

func wakeSelfUpgradeE2EEnv(root, stable, oldBinary, newBinary, oldVersion, newVersion string) []string {
	return wakeABICleanEnv(
		wakeRestartPTYOwnerHelperEnv+"=1",
		wakeSelfUpgradePTYOwnerHelperEnv+"=1",
		"AMQ_E2E_ROOT="+root,
		"AMQ_E2E_STABLE="+stable,
		"AMQ_E2E_OLD="+oldBinary,
		"AMQ_E2E_NEW="+newBinary,
		"AMQ_E2E_OLD_VERSION="+oldVersion,
		"AMQ_E2E_NEW_VERSION="+newVersion,
	)
}

func TestWakeSelfUpgradeRealPTYOwnerHelper(t *testing.T) {
	if os.Getenv(wakeSelfUpgradePTYOwnerHelperEnv) == "" {
		t.Skip("external self-upgrade PTY owner helper")
	}
	if !wakeInputIsTTY() {
		t.Fatal("self-upgrade owner helper stdin is not the retained PTY")
	}

	root := os.Getenv("AMQ_E2E_ROOT")
	stable := os.Getenv("AMQ_E2E_STABLE")
	newBinary := os.Getenv("AMQ_E2E_NEW")
	oldVersion := os.Getenv("AMQ_E2E_OLD_VERSION")
	newVersion := os.Getenv("AMQ_E2E_NEW_VERSION")
	if root == "" || stable == "" || newBinary == "" || oldVersion == "" || newVersion == "" {
		t.Fatal("self-upgrade owner helper environment is incomplete")
	}

	before := inspectWakeLock(root, "codex")
	if !before.Exists || before.Status != wakeLockValid || !before.IdentityConfirmed ||
		before.Lock.ImageVersion != oldVersion || before.Lock.RunningImageEvidence == nil {
		t.Fatalf("initial self-upgrade wake = %#v", before)
	}
	assertWakeSelfUpgradeCheckEligible(t, newBinary, root, stable)

	replacement := stable + ".next"
	if err := os.Symlink(newBinary, replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, stable); err != nil {
		t.Fatal(err)
	}

	requestedImage, err := captureWakeImageEvidence(newBinary, newVersion)
	if err != nil {
		t.Fatal(err)
	}
	after := waitForWakeSelfUpgradeReplacement(t, newBinary, root, before, requestedImage, newVersion)
	if after.PID != before.PID || after.Lock.Generation == before.Lock.Generation ||
		after.Lock.ImageVersion != newVersion || after.Lock.RunningImageEvidence == nil ||
		after.Lock.ImagePath != after.Lock.RunningImageEvidence.ExecutionPath ||
		!sameRequestedAndBoundWakeImageEvidence(requestedImage, *after.Lock.RunningImageEvidence) {
		t.Fatalf("self-upgrade replacement before=%#v after=%#v", before, after)
	}

	// A replacement is a single handoff, not a repeating signal loop. Give the
	// new wake time to settle and require its generation to remain unchanged.
	time.Sleep(2 * time.Second)
	settled := inspectWakeLock(root, "codex")
	if !sameWakeLockGeneration(after, settled) {
		t.Fatalf("self-upgrade performed more than one replacement: first=%#v settled=%#v", after, settled)
	}
	if _, err := os.Lstat(filepath.Join(root, "agents", "codex", wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("self-upgrade restart record survived replacement: %v", err)
	}

	drained := runWakeSelfUpgradeCommand(t, newBinary, "drain", "--root", root, "--me", "codex", "--include-body")
	if !strings.Contains(drained, "Subject: before-self-upgrade") ||
		!strings.Contains(drained, "preserve this unread message") {
		t.Fatalf("self-upgrade did not preserve pre-flip unread work:\n%s", drained)
	}
	assertWakeSelfUpgradeCheckCurrent(t, newBinary, root)
	fmt.Printf(
		"AMQ_SELF_UPGRADE_HELPER_OK pid=%d old_generation=%s new_generation=%s image=%s\n",
		after.PID,
		before.Lock.Generation,
		after.Lock.Generation,
		after.Lock.ImagePath,
	)
}

func TestWakeSelfUpgradePinnedDirectBinaryIsIneligible(t *testing.T) {
	if testing.Short() {
		t.Skip("build real pinned binary")
	}
	repoRoot := wakeSelfUpgradeE2ERepoRoot(t)
	binary := filepath.Join(t.TempDir(), "amq")
	buildVersionedWakeRestartBinary(t, repoRoot, binary, "0.58.0")
	running, err := captureWakeImageEvidence(binary, "0.58.0")
	if err != nil {
		t.Fatal(err)
	}
	state := captureWakeSelfUpgradeStartupState(binary, true, running)
	if state.Eligible || state.Locator != "" {
		t.Fatalf("direct binary self-upgrade state = %#v, want ineligible", state)
	}
}

func waitForWakeSelfUpgradeReplacement(
	t *testing.T,
	binary string,
	root string,
	before wakeLockInspection,
	requested wakeImageEvidenceV1,
	newVersion string,
) wakeLockInspection {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for {
		current := inspectWakeLock(root, "codex")
		if current.Exists && current.Status == wakeLockValid && current.IdentityConfirmed &&
			current.PID == before.PID && current.Lock.Generation != before.Lock.Generation &&
			current.Lock.ImageVersion == newVersion && current.Lock.RunningImageEvidence != nil &&
			sameRequestedAndBoundWakeImageEvidence(requested, *current.Lock.RunningImageEvidence) {
			return current
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"self-upgrade did not complete one maintenance-tick replacement; initial=%#v current=%#v\n%s",
				before,
				current,
				wakeSelfUpgradeTimeoutDiagnostics(binary, root),
			)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func wakeSelfUpgradeTimeoutDiagnostics(binary, root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "wake", "check", "--root", root, "--me", "codex", "--json", "--json-schema=2")
	cmd.Env = wakeABICleanEnv()
	output, err := cmd.CombinedOutput()
	check := wakeCheckResultV2{}
	if err == nil {
		if decodeErr := json.Unmarshal(output, &check); decodeErr != nil {
			err = fmt.Errorf("decode schema-v2 wake check: %w", decodeErr)
		}
	}
	return fmt.Sprintf(
		"self-upgrade timeout diagnostics: wake_check_self_upgrade=%#v wake_check_err=%v wake_check_raw=%q diagnostic_raw=%q restart_raw=%q",
		check.SelfUpgrade,
		err,
		truncateWakeSelfUpgradeE2ETestOutput(output),
		readWakeSelfUpgradeE2ETestFile(filepath.Join(root, "agents", "codex", wakeSelfUpgradeFileName)),
		readWakeSelfUpgradeE2ETestFile(filepath.Join(root, "agents", "codex", wakeRestartFileName)),
	)
}

func readWakeSelfUpgradeE2ETestFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 16*1024+1))
	if err != nil {
		return err.Error()
	}
	return truncateWakeSelfUpgradeE2ETestOutput(data)
}

func truncateWakeSelfUpgradeE2ETestOutput(value []byte) string {
	const limit = 16 * 1024
	if len(value) <= limit {
		return string(value)
	}
	return string(value[:limit]) + "[truncated]"
}

func assertWakeSelfUpgradeCheckCurrent(t *testing.T, binary, root string) {
	t.Helper()
	output := runWakeSelfUpgradeCommand(
		t,
		binary,
		"wake", "check", "--root", root, "--me", "codex", "--json", "--json-schema=2",
	)
	var check wakeCheckResultV2
	if err := json.Unmarshal([]byte(output), &check); err != nil {
		t.Fatalf("decode fresh self-upgrade wake check: %v\n%s", err, output)
	}
	if check.Image.Status != wakeImageCurrent || check.SelfUpgrade.RefusedMemory {
		t.Fatalf(
			"fresh self-upgrade wake check = %#v; doctor=%q",
			check,
			wakeSelfUpgradeExternalDoctorDiagnostics(binary, root),
		)
	}
}

func wakeSelfUpgradeExternalDoctorDiagnostics(binary, root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "doctor", "--ops", "--root", root, "--json", "--json-schema=2")
	cmd.Env = wakeABICleanEnv()
	output, err := cmd.CombinedOutput()
	var report struct {
		Ops struct {
			WakeLocks []json.RawMessage `json:"wake_locks"`
		} `json:"ops"`
	}
	if decodeErr := json.Unmarshal(output, &report); decodeErr != nil {
		return fmt.Sprintf(
			"err=%v decode=%v output=%s",
			err,
			decodeErr,
			truncateWakeSelfUpgradeE2ETestOutput(output),
		)
	}
	if len(report.Ops.WakeLocks) == 0 {
		return fmt.Sprintf("err=%v wake_locks=[]", err)
	}
	var lock struct {
		InspectionError string `json:"inspection_error"`
	}
	if decodeErr := json.Unmarshal(report.Ops.WakeLocks[0], &lock); decodeErr != nil {
		return fmt.Sprintf("err=%v wake_lock_decode=%v wake_lock=%s", err, decodeErr, report.Ops.WakeLocks[0])
	}
	return fmt.Sprintf(
		"err=%v inspection_error=%q wake_lock=%s",
		err,
		lock.InspectionError,
		truncateWakeSelfUpgradeE2ETestOutput(report.Ops.WakeLocks[0]),
	)
}

func assertWakeSelfUpgradeCheckEligible(t *testing.T, binary, root, stable string) {
	t.Helper()
	output := runWakeSelfUpgradeCommand(
		t,
		binary,
		"wake", "check", "--root", root, "--me", "codex", "--json", "--json-schema=2",
	)
	var check wakeCheckResultV2
	if err := json.Unmarshal([]byte(output), &check); err != nil {
		t.Fatalf("decode eligible self-upgrade wake check: %v\n%s", err, output)
	}
	if !check.SelfUpgrade.Enabled || !check.SelfUpgrade.Eligible || check.SelfUpgrade.Locator == nil ||
		*check.SelfUpgrade.Locator != stable || check.SelfUpgrade.RefusedMemory {
		t.Fatalf("eligible self-upgrade wake check = %#v", check)
	}
}

func runWakeSelfUpgradeCommand(t *testing.T, binary string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = wakeABICleanEnv()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("run %s %v timed out: %v\n%s", binary, args, ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("run %s %v: %v\n%s", binary, args, err, output)
	}
	return string(output)
}

func wakeSelfUpgradeE2ERepoRoot(t *testing.T) string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve self-upgrade E2E test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
}
