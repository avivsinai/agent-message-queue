//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type wakeCheckWire struct {
	Schema                   int    `json:"schema"`
	Agent                    string `json:"agent"`
	Root                     string `json:"root"`
	CanStartHere             bool   `json:"can_start_here"`
	StartMode                string `json:"start_mode"`
	StartReason              string `json:"start_reason"`
	LiveWake                 bool   `json:"live_wake"`
	WakeStatus               string `json:"wake_status"`
	WakePID                  int    `json:"wake_pid"`
	WakeMode                 string `json:"wake_mode"`
	OwnerBound               bool   `json:"owner_bound"`
	RunningImagePath         string `json:"running_image_path"`
	RunningVersion           string `json:"running_version"`
	CurrentImagePath         string `json:"current_image_path"`
	CurrentVersion           string `json:"current_version"`
	ImageStatus              string `json:"image_status"`
	CanRepairInjectVia       bool   `json:"can_repair_inject_via"`
	RepairReason             string `json:"repair_reason"`
	RestartCapability        string `json:"restart_capability"`
	OperatorTerminalRequired bool   `json:"operator_terminal_required"`
	NextAction               string `json:"next_action"`
}

func TestWakeCheckReportsAgentSafeDirectStartFromTTYWithoutMutation(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.49.14")
	before := snapshotWakeCheckTree(t, root)

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatalf("wake check: %v", err)
	}
	var got wakeCheckWire
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check JSON: %v\n%s", err, output)
	}
	if got.Schema != 1 || got.Agent != "codex" || got.Root != canonicalWakeRoot(root) {
		t.Fatalf("identity = %#v", got)
	}
	if !got.CanStartHere || got.StartMode != wakeInjectModeRaw || got.LiveWake {
		t.Fatalf("direct start capability = %#v", got)
	}
	if got.RestartCapability != "agent_safe" || got.OperatorTerminalRequired {
		t.Fatalf("restart capability = %#v", got)
	}
	wantAction := "amq wake --root " + shellQuoteArg(canonicalWakeRoot(root)) + " --me codex"
	if got.NextAction != wantAction {
		t.Fatalf("next_action = %q, want %q", got.NextAction, wantAction)
	}
	assertWakeCheckTreeUnchanged(t, root, before)
}

func TestWakeCheckReportsLiveStaleRawImageAndRequiresOwningTerminal(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	const (
		pid            = 4242
		runningPath    = "/opt/homebrew/Cellar/amq/0.49.12/bin/amq"
		runningVersion = "0.49.12"
	)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          "/dev/ttys005",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		Started:      "2026-07-31T01:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "live-raw-generation",
		ImagePath:    runningPath,
		ImageVersion: runningVersion,
	})
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        gotPID,
			Running:    true,
			StartToken: "12345",
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.49.14")
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonStartedMTime,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatalf("wake check: %v", err)
	}
	var got wakeCheckWire
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check JSON: %v\n%s", err, output)
	}
	if got.CanStartHere || !got.LiveWake || got.WakeStatus != string(wakeLockValid) ||
		got.WakePID != pid || got.WakeMode != wakeInjectModeRaw || got.OwnerBound {
		t.Fatalf("live wake = %#v", got)
	}
	if got.RunningImagePath != runningPath || got.RunningVersion != runningVersion ||
		got.CurrentVersion != "0.49.14" || got.ImageStatus != "different" {
		t.Fatalf("image evidence = %#v", got)
	}
	if got.RestartCapability != "operator_only" || !got.OperatorTerminalRequired {
		t.Fatalf("restart capability = %#v", got)
	}
	for _, want := range []string{"leave the live wake running", "owning terminal"} {
		if !strings.Contains(got.NextAction, want) {
			t.Fatalf("next_action missing %q: %q", want, got.NextAction)
		}
	}
	lockAfter, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockBefore, lockAfter) {
		t.Fatal("wake check changed the live lock")
	}
}

func TestWakeCheckReportsLegacyLiveImageEvidenceAsUnknown(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		TTY:          "/dev/ttys005",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		Started:      "2026-07-31T01:00:00Z",
		ProcessStart: "12345",
		BootID:       "legacy-boot",
		Executable:   "amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "legacy-live-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "12345",
			BootID:     "legacy-boot",
			Executable: "amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.49.14")
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{}, errors.New("legacy lock has no binary identity evidence")
	})

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatalf("wake check: %v", err)
	}
	var got wakeCheckWire
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check JSON: %v\n%s", err, output)
	}
	if !got.LiveWake || got.WakeStatus != string(wakeLockValid) {
		t.Fatalf("legacy live wake = %#v", got)
	}
	if got.RunningImagePath != wakeCheckUnknown ||
		got.RunningVersion != wakeCheckUnknown ||
		got.ImageStatus != wakeImageUnknown {
		t.Fatalf("legacy image evidence = %#v, want unknown", got)
	}
}

func TestDoctorOpsSharesWakeRestartAndImageEvidence(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(
		filepath.Join(root, "meta", "config.json"),
		config.Config{Version: 1, Agents: []string{"codex"}},
		true,
	); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		TTY:          "/dev/ttys005",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		Started:      "2026-07-31T01:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "doctor-live-raw-generation",
		ImagePath:    "/opt/homebrew/Cellar/amq/0.49.12/bin/amq",
		ImageVersion: "0.49.12",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        gotPID,
			Running:    true,
			StartToken: "12345",
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "amq",
		}
	})
	stubWakeCheckRuntime(t, false, "0.49.14")
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonStartedMTime,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	result := runOpsChecks(root, "test", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake locks = %#v", result.WakeLocks)
	}
	got := result.WakeLocks[0]
	if got.RunningImagePath != "/opt/homebrew/Cellar/amq/0.49.12/bin/amq" ||
		got.RunningVersion != "0.49.12" ||
		got.CurrentVersion != "0.49.14" ||
		got.ImageStatus != wakeImageDifferent {
		t.Fatalf("doctor image evidence = %#v", got)
	}
	if got.RestartCapability != wakeRestartOperatorOnly ||
		!got.OperatorTerminalRequired ||
		!strings.Contains(got.NextAction, "leave the live wake running") {
		t.Fatalf("doctor restart capability = %#v", got)
	}
}

func TestDoctorOpsDoesNotClaimCurrentImageFromVersionAlone(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(
		filepath.Join(root, "meta", "config.json"),
		config.Config{Version: 1, Agents: []string{"codex"}},
		true,
	); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "12345",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "same-version-generation",
		ImagePath:    "/opt/homebrew/bin/amq",
		ImageVersion: "0.49.14",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "12345",
			BootID:     "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.49.14")
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{}, errors.New("binary evidence unavailable")
	})

	result := runOpsChecks(root, "test", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake locks = %#v", result.WakeLocks)
	}
	if got := result.WakeLocks[0].ImageStatus; got != wakeImageUnknown {
		t.Fatalf("image_status = %q, want %q without binary identity evidence", got, wakeImageUnknown)
	}
}

func TestWakeCheckReportsEligibleInjectViaRepairAsAgentSafe(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	targetDigest, err := wakeTargetDigest(target)
	if err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		Started:      "2026-07-31T01:00:00Z",
		ProcessStart: "12345",
		BootID:       wakeRepairTestBootID,
		Executable:   "amq",
		WakeMode:     wakeTargetInjectVia,
		TargetDigest: targetDigest,
		Generation:   "stale-inject-via-generation",
	})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	writeWakeRepairFloorForTest(t, root, "codex", target, nil)
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		return wakeProcessInfo{PID: gotPID}
	})
	stubWakeCheckRuntime(t, false, "0.49.14")

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatalf("wake check: %v", err)
	}
	var got wakeCheckWire
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check JSON: %v\n%s", err, output)
	}
	if !got.CanRepairInjectVia || got.RestartCapability != wakeRestartAgentSafe {
		t.Fatalf("repair capability = %#v", got)
	}
	wantAction := wakeRepairCommand(canonicalWakeRoot(root), "codex")
	if got.NextAction != wantAction {
		t.Fatalf("next_action = %q, want %q", got.NextAction, wantAction)
	}
}

func TestWakeCheckRefusesCapabilityFromChangingRepairMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, wakeTarget, wakeRepairFloor)
	}{
		{
			name: "target changed",
			mutate: func(t *testing.T, root string, target wakeTarget, _ wakeRepairFloor) {
				t.Helper()
				target.InjectArgs = append(target.InjectArgs, "changed-during-check")
				if err := writeWakeTarget(root, "codex", target); err != nil {
					t.Fatalf("replace wake target: %v", err)
				}
			},
		},
		{
			name: "repair floor changed",
			mutate: func(t *testing.T, root string, _ wakeTarget, floor wakeRepairFloor) {
				t.Helper()
				floor.Existing["changed-during-check.md"] = wakeFileIdentity{
					Device: 1, Inode: 2, CTimeSec: 3, CTimeNsec: 4,
				}
				if err := writeWakeRepairFloor(root, "codex", floor); err != nil {
					t.Fatalf("replace wake repair floor: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			injector := writeExecutableForTest(t, "injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
			targetDigest, err := wakeTargetDigest(target)
			if err != nil {
				t.Fatal(err)
			}
			writeWakeLockForTest(t, root, "codex", wakeLock{
				PID:          4242,
				Root:         canonicalWakeRoot(root),
				Agent:        "codex",
				Started:      "2026-07-31T01:00:00Z",
				ProcessStart: "12345",
				BootID:       wakeRepairTestBootID,
				Executable:   "amq",
				WakeMode:     wakeTargetInjectVia,
				TargetDigest: targetDigest,
				Generation:   "stale-inject-via-generation",
			})
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatal(err)
			}
			floor := writeWakeRepairFloorForTest(t, root, "codex", target, nil)

			inspections := 0
			stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
				inspections++
				if inspections == 3 {
					test.mutate(t, root, target, floor)
				}
				return wakeProcessInfo{PID: gotPID}
			})
			stubWakeCheckRuntime(t, false, "0.49.14")

			got := inspectWakeCheck(root, "codex")
			if got.RestartCapability != wakeRestartUnavailable ||
				got.CanRepairInjectVia ||
				got.NextAction != "wake state changed during inspection; retry amq wake check" {
				t.Fatalf("changing repair metadata capability = %#v", got)
			}
		})
	}
}

func TestWakeCheckPreservesUnverifiedWakeState(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		ProcessStart: "same-start",
		BootID:       "recorded-boot",
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "unverified-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "same-start",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, true, "0.49.14")
	before := snapshotWakeCheckTree(t, root)

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatalf("wake check: %v", err)
	}
	var got wakeCheckWire
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check JSON: %v\n%s", err, output)
	}
	if got.WakeStatus != string(wakeLockUnverified) ||
		got.RestartCapability != wakeRestartUnavailable ||
		got.NextAction != "preserve the unverified wake state and inspect it with amq doctor --ops" {
		t.Fatalf("unverified capability = %#v", got)
	}
	assertWakeCheckTreeUnchanged(t, root, before)
}

func TestWakeCheckRefusesAttentionOnlyDowngrade(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.49.14")
	readTIOCSTILegacySysctl = func() ([]byte, error) { return []byte("0\n"), nil }

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatalf("wake check: %v", err)
	}
	var got wakeCheckWire
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check JSON: %v\n%s", err, output)
	}
	if got.CanStartHere || got.StartMode != wakeInjectModeNone ||
		got.RestartCapability != wakeRestartUnavailable ||
		!strings.Contains(got.NextAction, "do not accept an attention-only downgrade") {
		t.Fatalf("attention-only capability = %#v", got)
	}
}

func TestWakeCheckRefusesCapabilityFromChangingWakeState(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	baseLock := wakeLock{
		PID:          4242,
		TTY:          "/dev/ttys005",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		Started:      "2026-07-31T01:00:00Z",
		ProcessStart: "same-start",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "first-generation",
	}
	writeWakeLockForTest(t, root, "codex", baseLock)
	inspections := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspections++
		if inspections == 2 {
			replacement := baseLock
			replacement.Generation = "replacement-generation"
			writeWakeLockExactForTest(t, root, "codex", replacement)
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "same-start",
			BootID:     "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.49.14")
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{}, nil
	})

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatalf("wake check: %v", err)
	}
	var got wakeCheckWire
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check JSON: %v\n%s", err, output)
	}
	if got.RestartCapability != wakeRestartUnavailable ||
		got.CanRepairInjectVia ||
		got.NextAction != "wake state changed during inspection; retry amq wake check" {
		t.Fatalf("changing wake capability = %#v", got)
	}
}

func TestWakeCheckTextPrintsExactClassifiedAction(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.49.14")
	result := inspectWakeCheck(root, "codex")

	output, err := captureEnvStdout(t, func() error {
		return writeWakeCheckText(result)
	})
	if err != nil {
		t.Fatalf("write wake check text: %v", err)
	}
	for _, want := range []string{
		"Restart capability: " + result.RestartCapability,
		"Next action: " + result.NextAction,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("wake check text missing %q:\n%s", want, output)
		}
	}
}

func TestNewWakeLockCapturesRunningImageEvidence(t *testing.T) {
	root := secureTempDirForTest(t)
	oldVersion := cliVersion
	cliVersion = "0.49.14-test"
	t.Cleanup(func() { cliVersion = oldVersion })

	lock, err := newWakeLock(root, "codex", wakeLockAcquireOptions{
		wakeMode: wakeInjectModeRaw,
	})
	if err != nil {
		t.Fatalf("new wake lock: %v", err)
	}
	if lock.ImagePath == "" || !filepath.IsAbs(lock.ImagePath) {
		t.Fatalf("image_path = %q, want absolute path", lock.ImagePath)
	}
	if lock.ImageVersion != "0.49.14-test" {
		t.Fatalf("image_version = %q", lock.ImageVersion)
	}
}

func stubWakeCheckRuntime(t *testing.T, tty bool, version string) {
	t.Helper()
	oldAvailable := wakeTIOCSTIAvailable
	oldTTY := wakeInputIsTTY
	oldRead := readTIOCSTILegacySysctl
	oldVersion := cliVersion
	oldResolve := resolveWakeExecutablePath
	currentPath := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(currentPath, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	wakeTIOCSTIAvailable = func() bool { return true }
	wakeInputIsTTY = func() bool { return tty }
	readTIOCSTILegacySysctl = func() ([]byte, error) { return nil, os.ErrNotExist }
	cliVersion = version
	resolveWakeExecutablePath = func() (string, string, error) {
		return currentPath, currentPath, nil
	}
	t.Cleanup(func() {
		wakeTIOCSTIAvailable = oldAvailable
		wakeInputIsTTY = oldTTY
		readTIOCSTILegacySysctl = oldRead
		cliVersion = oldVersion
		resolveWakeExecutablePath = oldResolve
	})
}

func snapshotWakeCheckTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snapshot := map[string][]byte{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[path] = append([]byte(nil), data...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertWakeCheckTreeUnchanged(t *testing.T, root string, before map[string][]byte) {
	t.Helper()
	after := snapshotWakeCheckTree(t, root)
	if len(after) != len(before) {
		t.Fatalf("wake check changed file count: before=%d after=%d", len(before), len(after))
	}
	for path, want := range before {
		got, ok := after[path]
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("wake check changed %s", path)
		}
	}
}
