//go:build darwin || linux

package cli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
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
	writeWakeLockForTest(t, root, "codex", wakeLock{
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
	assertWakeCheckTreeUnchanged(t, root, before)
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
	if !got.LiveWake || got.WakeStatus != string(wakeLockValid) {
		t.Fatalf("legacy live wake = %#v", got)
	}
	if got.RunningImagePath != wakeCheckUnknown ||
		got.RunningVersion != wakeCheckUnknown ||
		got.ImageStatus != wakeImageUnknown {
		t.Fatalf("legacy image evidence = %#v, want unknown", got)
	}
	assertWakeCheckTreeUnchanged(t, root, before)
}

func TestWakeCheckImageStatusRequiresCompleteExactEvidence(t *testing.T) {
	tests := []struct {
		name       string
		lock       wakeLock
		result     wakeCheckResult
		comparison wakeBinaryStaleness
		want       string
	}{
		{
			name: "equal versions with non-exact comparison stay unknown",
			lock: wakeLock{
				ImagePath:    "/opt/homebrew/bin/amq",
				ImageVersion: "0.49.14",
			},
			result: wakeCheckResult{
				RunningVersion: "0.49.14",
				CurrentVersion: "0.49.14",
			},
			comparison: wakeBinaryStaleness{
				Method:   wakeBinaryComparisonStartedMTime,
				Evidence: stableWakeBinaryEvidenceForTest(),
			},
			want: wakeImageUnknown,
		},
		{
			name: "complete lock with Darwin mapped-vnode agreement is current",
			lock: wakeLock{
				ImagePath:    "/opt/homebrew/bin/amq",
				ImageVersion: "0.49.14",
			},
			result: wakeCheckResult{
				RunningVersion: "0.49.14",
				CurrentVersion: "0.49.14",
			},
			comparison: wakeBinaryStaleness{
				Method:   wakeBinaryComparisonDarwinProcessImage,
				Evidence: stableWakeBinaryEvidenceForTest(),
			},
			want: wakeImageCurrent,
		},
		{
			name: "legacy lock stays unknown despite exact comparison",
			result: wakeCheckResult{
				RunningVersion: wakeCheckUnknown,
				CurrentVersion: "0.49.14",
			},
			comparison: wakeBinaryStaleness{
				Method:   wakeBinaryComparisonExactIdentity,
				Evidence: stableWakeBinaryEvidenceForTest(),
			},
			want: wakeImageUnknown,
		},
		{
			name: "complete lock with exact comparison is current",
			lock: wakeLock{
				ImagePath:    "/opt/homebrew/bin/amq",
				ImageVersion: "0.49.14",
			},
			result: wakeCheckResult{
				RunningVersion: "0.49.14",
				CurrentVersion: "0.49.14",
			},
			comparison: wakeBinaryStaleness{
				Method:   wakeBinaryComparisonExactIdentity,
				Evidence: stableWakeBinaryEvidenceForTest(),
			},
			want: wakeImageCurrent,
		},
		{
			name: "version conflict wins over exact comparison",
			lock: wakeLock{
				ImagePath:    "/opt/homebrew/bin/amq",
				ImageVersion: "0.49.13",
			},
			result: wakeCheckResult{
				RunningVersion: "0.49.13",
				CurrentVersion: "0.49.14",
			},
			comparison: wakeBinaryStaleness{
				Method:   wakeBinaryComparisonExactIdentity,
				Evidence: stableWakeBinaryEvidenceForTest(),
			},
			want: wakeImageDifferent,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
				return test.comparison, nil
			})

			got := inspectWakeCheckImageStatus(
				wakeLockInspection{Lock: test.lock},
				test.result,
			)
			if got != test.want {
				t.Fatalf("image status = %q, want %q", got, test.want)
			}
		})
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
	if !got.CanRepairInjectVia || got.RestartCapability != wakeRestartAgentSafe {
		t.Fatalf("repair capability = %#v", got)
	}
	wantAction := wakeRepairCommand(canonicalWakeRoot(root), "codex")
	if got.NextAction != wantAction {
		t.Fatalf("next_action = %q, want %q", got.NextAction, wantAction)
	}
	assertWakeCheckTreeUnchanged(t, root, before)
}

func TestWakeCheckStaleNonrepairableWithoutInjectorIsUnavailable(t *testing.T) {
	tests := []struct {
		name      string
		configure func()
	}{
		{
			name: "TIOCSTI unavailable",
			configure: func() {
				wakeTIOCSTIAvailable = func() bool { return false }
			},
		},
		{
			name: "TIOCSTI disabled",
			configure: func() {
				readTIOCSTILegacySysctl = func() ([]byte, error) { return []byte("0\n"), nil }
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			writeWakeLockForTest(t, root, "codex", wakeLock{
				PID:          4242,
				Root:         canonicalWakeRoot(root),
				Agent:        "codex",
				ProcessStart: "12345",
				BootID:       "stale-boot",
				Executable:   "amq",
				WakeMode:     wakeInjectModeRaw,
				Generation:   "stale-raw-generation",
			})
			stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
				return wakeProcessInfo{PID: gotPID}
			})
			stubWakeCheckRuntime(t, true, "0.49.14")
			test.configure()
			before := snapshotWakeCheckTree(t, root)

			result := inspectWakeCheck(root, "codex")

			if result.WakeStatus != string(wakeLockStale) ||
				result.StartMode != wakeInjectModeNone ||
				result.RestartCapability != wakeRestartUnavailable ||
				result.OperatorTerminalRequired ||
				!strings.Contains(result.NextAction, "restore a supported full-strength injector") {
				t.Fatalf("stale no-injector capability = %#v", result)
			}
			if strings.Contains(result.NextAction, "then run amq wake") {
				t.Fatalf("stale no-injector action offered raw fallback: %q", result.NextAction)
			}
			assertWakeCheckTreeUnchanged(t, root, before)
		})
	}
}

func TestWakeCheckStaleInjectViaMissingFloorRequiresInjectorRestoration(t *testing.T) {
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
		Generation:   "stale-missing-floor-generation",
	})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		return wakeProcessInfo{PID: gotPID}
	})
	stubWakeCheckRuntime(t, false, "0.49.14")
	before := snapshotWakeCheckTree(t, root)

	got := inspectWakeCheck(root, "codex")
	if got.CanRepairInjectVia || !strings.Contains(got.RepairReason, "repair floor is missing") {
		t.Fatalf("missing-floor repair evidence = %#v", got)
	}
	if got.RestartCapability != wakeRestartUnavailable || got.OperatorTerminalRequired {
		t.Fatalf("missing-floor restart capability = %#v", got)
	}
	for _, want := range []string{"restore", "--inject-via", "do not fall back"} {
		if !strings.Contains(got.NextAction, want) {
			t.Fatalf("missing-floor next action missing %q: %q", want, got.NextAction)
		}
	}
	assertWakeCheckTreeUnchanged(t, root, before)
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
			var expectedAfterChange map[string]wakeCheckTreeEntry
			stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
				inspections++
				if inspections == 3 {
					test.mutate(t, root, target, floor)
					expectedAfterChange = snapshotWakeCheckTree(t, root)
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
			if expectedAfterChange == nil {
				t.Fatal("test did not capture the injected metadata change")
			}
			assertWakeCheckTreeUnchanged(t, root, expectedAfterChange)
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
	if got.CanStartHere || got.StartMode != wakeInjectModeNone ||
		got.RestartCapability != wakeRestartUnavailable ||
		!strings.Contains(got.NextAction, "do not accept an attention-only downgrade") {
		t.Fatalf("attention-only capability = %#v", got)
	}
	assertWakeCheckTreeUnchanged(t, root, before)
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
	var expectedAfterChange map[string]wakeCheckTreeEntry
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspections++
		if inspections == 2 {
			replacement := baseLock
			replacement.Generation = "replacement-generation"
			writeWakeLockExactForTest(t, root, "codex", replacement)
			expectedAfterChange = snapshotWakeCheckTree(t, root)
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
	if expectedAfterChange == nil {
		t.Fatal("test did not capture the injected lock change")
	}
	assertWakeCheckTreeUnchanged(t, root, expectedAfterChange)
}

func TestWakeCheckRefusesImageStatusWhenWakeChangesDuringImageProbe(t *testing.T) {
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
		Generation:   "probe-generation",
		ImagePath:    "/opt/homebrew/bin/amq",
		ImageVersion: "0.49.14",
	}
	writeWakeLockForTest(t, root, "codex", baseLock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
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
		replacement := baseLock
		replacement.Generation = "replacement-during-probe"
		writeWakeLockExactForTest(t, root, "codex", replacement)
		return wakeBinaryStaleness{
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	got := inspectWakeCheck(root, "codex")
	if got.ImageStatus != wakeImageUnknown ||
		got.NextAction != "wake state changed during inspection; retry amq wake check" {
		t.Fatalf("wake changed during image probe = %#v", got)
	}
}

func TestWakeCheckTextPrintsExactClassifiedAction(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.49.14")
	before := snapshotWakeCheckTree(t, root)
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
	assertWakeCheckTreeUnchanged(t, root, before)
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
	if runtime.GOOS != "darwin" {
		if lock.RunningImageEvidence != nil {
			t.Fatalf("non-Darwin wake lock unexpectedly has running image evidence: %#v", lock.RunningImageEvidence)
		}
		return
	}
	if lock.RunningImageEvidence == nil {
		t.Fatal("new wake lock is missing running image evidence")
	}
	if lock.RunningImageEvidence.ExecutionPath != lock.ImagePath ||
		lock.RunningImageEvidence.EmbeddedVersion != lock.ImageVersion {
		t.Fatalf("running image evidence does not bind path/version: %#v", lock)
	}
}

func TestSnapshotWakeCheckTreeCapturesFullNoFollowFingerprint(t *testing.T) {
	root := secureTempDirForTest(t)
	emptyDir := filepath.Join(root, "empty")
	if err := os.Mkdir(emptyDir, 0o750); err != nil {
		t.Fatal(err)
	}
	regularPath := filepath.Join(root, "regular")
	regularContent := []byte("regular-content")
	if err := os.WriteFile(regularPath, regularContent, 0o640); err != nil {
		t.Fatal(err)
	}
	externalPath := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(externalPath, []byte("external-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalPath, filepath.Join(root, "external-link")); err != nil {
		t.Fatal(err)
	}

	before := snapshotWakeCheckTree(t, root)
	for _, rel := range []string{".", "empty", "regular", "external-link"} {
		entry, ok := before[rel]
		if !ok || entry.RelativePath != rel {
			t.Fatalf("snapshot entry %q = (%#v, %t)", rel, entry, ok)
		}
	}
	rootEntry := before["."]
	if rootEntry.Type != os.ModeDir || rootEntry.Mode.Perm() == 0 {
		t.Fatalf("root fingerprint = %#v", rootEntry)
	}
	if empty := before["empty"]; empty.Type != os.ModeDir || empty.Mode.Perm() != 0o750 {
		t.Fatalf("empty directory fingerprint = %#v", empty)
	}
	regular := before["regular"]
	if regular.Type != 0 || regular.Mode.Perm() != 0o640 ||
		regular.Identity.Device == 0 || regular.Identity.Inode == 0 ||
		regular.Identity.CTimeSec == 0 || !regular.OwnerKnown ||
		regular.UID != uint32(os.Geteuid()) || regular.GID != uint32(os.Getegid()) ||
		regular.Size != int64(len(regularContent)) ||
		regular.ModTimeUnixNano == 0 || !regular.ContentDigestKnown ||
		regular.ContentDigest != sha256.Sum256(regularContent) {
		t.Fatalf("regular fingerprint = %#v", regular)
	}
	link := before["external-link"]
	if link.Type != os.ModeSymlink || link.SymlinkTarget != externalPath ||
		link.ContentDigestKnown {
		t.Fatalf("symlink fingerprint = %#v", link)
	}

	if err := os.WriteFile(externalPath, []byte("external-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := snapshotWakeCheckTree(t, root)
	if after["external-link"] != link {
		t.Fatalf("symlink fingerprint followed target content: before=%#v after=%#v", link, after["external-link"])
	}
}

func TestSnapshotWakeCheckTreeStaysPinnedWhenCanonicalRootIsReplaced(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	originalContent := []byte("original-tree")
	if err := os.WriteFile(filepath.Join(root, "original"), originalContent, 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(parent, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "foreign"), []byte("must-not-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	movedRoot := filepath.Join(parent, "retained-root")

	oldHook := wakeCheckSnapshotAfterRootOpen
	wakeCheckSnapshotAfterRootOpen = func() error {
		if err := os.Rename(root, movedRoot); err != nil {
			return err
		}
		return os.Symlink(foreign, root)
	}
	t.Cleanup(func() { wakeCheckSnapshotAfterRootOpen = oldHook })

	snapshot := snapshotWakeCheckTree(t, root)
	original, ok := snapshot["original"]
	if !ok || !original.ContentDigestKnown ||
		original.ContentDigest != sha256.Sum256(originalContent) {
		t.Fatalf("retained original fingerprint = (%#v, %t)", original, ok)
	}
	if _, ok := snapshot["foreign"]; ok {
		t.Fatalf("snapshot escaped into foreign replacement: %#v", snapshot)
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

type wakeCheckTreeEntry struct {
	RelativePath       string
	Type               os.FileMode
	Mode               os.FileMode
	Identity           wakeFileIdentity
	UID                uint32
	GID                uint32
	OwnerKnown         bool
	Size               int64
	ModTimeUnixNano    int64
	SymlinkTarget      string
	ContentDigest      [sha256.Size]byte
	ContentDigestKnown bool
}

var wakeCheckSnapshotAfterRootOpen = func() error { return nil }

func snapshotWakeCheckTree(t *testing.T, root string) map[string]wakeCheckTreeEntry {
	t.Helper()
	var pathBefore unix.Stat_t
	if err := unix.Lstat(root, &pathBefore); err != nil {
		t.Fatal(err)
	}
	if uint32(pathBefore.Mode)&unix.S_IFMT != unix.S_IFDIR {
		t.Fatalf("wake check snapshot root %s is not a directory", root)
	}
	rootFD, err := unix.Open(
		root,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(rootFD) }()

	var openedBefore, pathAfter unix.Stat_t
	if err := unix.Fstat(rootFD, &openedBefore); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lstat(root, &pathAfter); err != nil {
		t.Fatal(err)
	}
	if !sameWakeCheckTreeStat(&pathBefore, &openedBefore) ||
		!sameWakeCheckTreeStat(&openedBefore, &pathAfter) {
		t.Fatalf("wake check snapshot root %s changed while it was opened", root)
	}
	if err := wakeCheckSnapshotAfterRootOpen(); err != nil {
		t.Fatal(err)
	}

	// From this point forward the retained descriptor, rather than the canonical
	// pathname, is the tree capability. A test hook may deliberately rename the
	// directory after the secure open; capture the post-hook metadata so the
	// final stability check permits that rename but rejects later mutation.
	var retainedBefore unix.Stat_t
	if err := unix.Fstat(rootFD, &retainedBefore); err != nil {
		t.Fatal(err)
	}
	if !sameWakeCheckTreeObject(&openedBefore, &retainedBefore) {
		t.Fatalf("wake check snapshot root %s changed identity after it was opened", root)
	}

	snapshot := map[string]wakeCheckTreeEntry{}
	snapshot["."] = fingerprintWakeCheckTreeStat(".", &retainedBefore)
	err = snapshotWakeCheckDirAt(rootFD, ".", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var retainedAfter unix.Stat_t
	if err := unix.Fstat(rootFD, &retainedAfter); err != nil {
		t.Fatal(err)
	}
	if !sameWakeCheckTreeStat(&retainedBefore, &retainedAfter) {
		t.Fatalf("wake check snapshot root %s changed during traversal", root)
	}
	return snapshot
}

func snapshotWakeCheckDirAt(
	dirfd int,
	rel string,
	snapshot map[string]wakeCheckTreeEntry,
) error {
	scanFD, err := unix.Dup(dirfd)
	if err != nil {
		return fmt.Errorf("duplicate retained directory %s: %w", rel, err)
	}
	unix.CloseOnExec(scanFD)
	scan := os.NewFile(uintptr(scanFD), "wake-check-snapshot:"+rel)
	if scan == nil {
		_ = unix.Close(scanFD)
		return fmt.Errorf("wrap retained directory %s", rel)
	}
	entries, readErr := scan.ReadDir(-1)
	closeErr := scan.Close()
	if readErr != nil {
		return fmt.Errorf("enumerate retained directory %s: %w", rel, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close retained directory scan %s: %w", rel, closeErr)
	}

	for _, directoryEntry := range entries {
		name := directoryEntry.Name()
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			return fmt.Errorf("invalid entry name %q under %s", name, rel)
		}
		childRel := name
		if rel != "." {
			childRel = rel + "/" + name
		}
		if err := snapshotWakeCheckChildAt(dirfd, name, childRel, snapshot); err != nil {
			return err
		}
	}
	return nil
}

func snapshotWakeCheckChildAt(
	dirfd int,
	name, rel string,
	snapshot map[string]wakeCheckTreeEntry,
) error {
	var before unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat %s without following symlinks: %w", rel, err)
	}
	entry := fingerprintWakeCheckTreeStat(rel, &before)

	switch uint32(before.Mode) & unix.S_IFMT {
	case unix.S_IFREG:
		digest, err := digestWakeCheckRegularFileAt(dirfd, name, rel, &before)
		if err != nil {
			return err
		}
		entry.ContentDigest = digest
		entry.ContentDigestKnown = true
	case unix.S_IFDIR:
		childFD, err := unix.Openat(
			dirfd,
			name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return fmt.Errorf("open retained directory %s: %w", rel, err)
		}
		var opened unix.Stat_t
		if err := unix.Fstat(childFD, &opened); err != nil {
			_ = unix.Close(childFD)
			return fmt.Errorf("stat retained directory %s: %w", rel, err)
		}
		if !sameWakeCheckTreeStat(&before, &opened) {
			_ = unix.Close(childFD)
			return fmt.Errorf("directory %s changed before traversal", rel)
		}
		snapshot[rel] = entry
		walkErr := snapshotWakeCheckDirAt(childFD, rel, snapshot)
		var openedAfter unix.Stat_t
		statErr := unix.Fstat(childFD, &openedAfter)
		closeErr := unix.Close(childFD)
		if walkErr != nil {
			return walkErr
		}
		if statErr != nil {
			return fmt.Errorf("re-stat retained directory %s: %w", rel, statErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close retained directory %s: %w", rel, closeErr)
		}
		var namedAfter unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &namedAfter, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("re-stat directory %s without following symlinks: %w", rel, err)
		}
		if !sameWakeCheckTreeStat(&opened, &openedAfter) ||
			!sameWakeCheckTreeStat(&openedAfter, &namedAfter) {
			return fmt.Errorf("directory %s changed during traversal", rel)
		}
		return nil
	case unix.S_IFLNK:
		target, err := readWakeCheckSymlinkAt(dirfd, name, rel)
		if err != nil {
			return err
		}
		entry.SymlinkTarget = target
	}

	var after unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("re-stat %s without following symlinks: %w", rel, err)
	}
	if !sameWakeCheckTreeStat(&before, &after) {
		return fmt.Errorf("entry %s changed during snapshot", rel)
	}
	snapshot[rel] = entry
	return nil
}

func fingerprintWakeCheckTreeStat(rel string, stat *unix.Stat_t) wakeCheckTreeEntry {
	mode := wakeCheckSnapshotFileMode(uint32(stat.Mode))
	mtimeSec, mtimeNsec, ctimeSec, ctimeNsec := wakeCheckSnapshotStatTimes(stat)
	return wakeCheckTreeEntry{
		RelativePath: rel,
		Type:         mode.Type(),
		Mode:         mode,
		Identity: wakeFileIdentity{
			Device:    uint64(stat.Dev),
			Inode:     uint64(stat.Ino),
			CTimeSec:  ctimeSec,
			CTimeNsec: ctimeNsec,
		},
		UID:             stat.Uid,
		GID:             stat.Gid,
		OwnerKnown:      true,
		Size:            stat.Size,
		ModTimeUnixNano: mtimeSec*int64(1e9) + mtimeNsec,
	}
}

func wakeCheckSnapshotFileMode(raw uint32) os.FileMode {
	mode := os.FileMode(raw & 0o777)
	if raw&unix.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if raw&unix.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if raw&unix.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	switch raw & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= os.ModeDir
	case unix.S_IFLNK:
		mode |= os.ModeSymlink
	case unix.S_IFIFO:
		mode |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= os.ModeSocket
	case unix.S_IFBLK:
		mode |= os.ModeDevice
	case unix.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	}
	return mode
}

func digestWakeCheckRegularFileAt(
	dirfd int,
	name, rel string,
	before *unix.Stat_t,
) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	fd, err := unix.Openat(
		dirfd,
		name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return zero, fmt.Errorf("open regular file %s without following symlinks: %w", rel, err)
	}
	file := os.NewFile(uintptr(fd), "wake-check-snapshot:"+rel)
	if file == nil {
		_ = unix.Close(fd)
		return zero, fmt.Errorf("wrap regular file %s", rel)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = file.Close()
		return zero, fmt.Errorf("stat opened regular file %s: %w", rel, err)
	}
	if uint32(opened.Mode)&unix.S_IFMT != unix.S_IFREG ||
		!sameWakeCheckTreeStat(before, &opened) {
		_ = file.Close()
		return zero, fmt.Errorf("regular file %s changed before content read", rel)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		_ = file.Close()
		return zero, fmt.Errorf("digest regular file %s: %w", rel, err)
	}
	var openedAfter unix.Stat_t
	if err := unix.Fstat(fd, &openedAfter); err != nil {
		_ = file.Close()
		return zero, fmt.Errorf("re-stat opened regular file %s: %w", rel, err)
	}
	if err := file.Close(); err != nil {
		return zero, fmt.Errorf("close regular file %s: %w", rel, err)
	}
	var namedAfter unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &namedAfter, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return zero, fmt.Errorf("re-stat regular file %s without following symlinks: %w", rel, err)
	}
	if !sameWakeCheckTreeStat(&opened, &openedAfter) ||
		!sameWakeCheckTreeStat(&openedAfter, &namedAfter) {
		return zero, fmt.Errorf("regular file %s changed during content read", rel)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

func readWakeCheckSymlinkAt(dirfd int, name, rel string) (string, error) {
	for size := 256; size <= 1<<20; size *= 2 {
		buffer := make([]byte, size)
		n, err := unix.Readlinkat(dirfd, name, buffer)
		if err != nil {
			return "", fmt.Errorf("read symlink %s: %w", rel, err)
		}
		if n < len(buffer) {
			return string(buffer[:n]), nil
		}
	}
	return "", fmt.Errorf("symlink %s target exceeds snapshot limit", rel)
}

func sameWakeCheckTreeObject(first, second *unix.Stat_t) bool {
	return first != nil && second != nil &&
		uint64(first.Dev) == uint64(second.Dev) &&
		uint64(first.Ino) == uint64(second.Ino)
}

func wakeCheckSnapshotStatTimes(stat *unix.Stat_t) (
	mtimeSec, mtimeNsec, ctimeSec, ctimeNsec int64,
) {
	return int64(stat.Mtim.Sec), int64(stat.Mtim.Nsec),
		int64(stat.Ctim.Sec), int64(stat.Ctim.Nsec)
}

func sameWakeCheckTreeStat(first, second *unix.Stat_t) bool {
	if !sameWakeCheckTreeObject(first, second) ||
		uint32(first.Mode) != uint32(second.Mode) ||
		first.Uid != second.Uid ||
		first.Gid != second.Gid ||
		first.Size != second.Size {
		return false
	}
	firstMTimeSec, firstMTimeNsec, firstCTimeSec, firstCTimeNsec := wakeCheckSnapshotStatTimes(first)
	secondMTimeSec, secondMTimeNsec, secondCTimeSec, secondCTimeNsec := wakeCheckSnapshotStatTimes(second)
	return firstMTimeSec == secondMTimeSec &&
		firstMTimeNsec == secondMTimeNsec &&
		firstCTimeSec == secondCTimeSec &&
		firstCTimeNsec == secondCTimeNsec
}

func assertWakeCheckTreeUnchanged(
	t *testing.T,
	root string,
	before map[string]wakeCheckTreeEntry,
) {
	t.Helper()
	after := snapshotWakeCheckTree(t, root)
	if len(after) != len(before) {
		t.Fatalf("wake check changed file count: before=%d after=%d", len(before), len(after))
	}
	for path, want := range before {
		got, ok := after[path]
		if !ok || got != want {
			t.Fatalf("wake check changed %s: before=%#v after=%#v present=%t", path, want, got, ok)
		}
	}
}

func TestSameWakeCheckInspectionExistenceEachClause(t *testing.T) {
	missing := wakeLockInspection{}
	present := wakeLockInspection{Exists: true}
	if !sameWakeCheckInspection(missing, wakeLockInspection{}) {
		t.Fatal("two missing inspections not equal")
	}
	if sameWakeCheckInspection(missing, present) || sameWakeCheckInspection(present, missing) {
		t.Fatal("missing vs present inspections treated as equal")
	}
	stale := wakeLockInspection{Exists: true, Status: wakeLockStale}
	valid := wakeLockInspection{Exists: true, Status: wakeLockValid}
	if sameWakeCheckInspection(valid, stale) {
		t.Fatal("status mismatch treated as equal")
	}
}

func TestInspectWakeCheckImageStatusRequiresPathAndVersion(t *testing.T) {
	mismatch := wakeCheckResult{RunningVersion: "1.0.0", CurrentVersion: "2.0.0"}
	if got := inspectWakeCheckImageStatus(wakeLockInspection{Lock: wakeLock{ImagePath: "/bin/amq"}}, mismatch); got != wakeImageUnknown {
		t.Fatalf("empty version = %q, want unknown", got)
	}
	if got := inspectWakeCheckImageStatus(wakeLockInspection{Lock: wakeLock{ImageVersion: "1.0.0"}}, mismatch); got != wakeImageUnknown {
		t.Fatalf("empty path = %q, want unknown", got)
	}
	if got := inspectWakeCheckImageStatus(wakeLockInspection{
		Lock: wakeLock{ImagePath: "/bin/amq", ImageVersion: "1.0.0"},
	}, mismatch); got != wakeImageDifferent {
		t.Fatalf("path and version set = %q, want different", got)
	}
}
