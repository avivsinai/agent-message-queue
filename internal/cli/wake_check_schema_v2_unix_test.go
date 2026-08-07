//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeCheckJSONSchemaV1GoldenBytes(t *testing.T) {
	result := wakeCheckResult{
		Schema:                   1,
		Agent:                    "codex",
		Root:                     "/queue",
		CanStartHere:             false,
		StartMode:                "raw",
		StartReason:              "owning terminal required",
		LiveWake:                 true,
		WakeStatus:               "valid",
		WakePID:                  42,
		WakeMode:                 "raw",
		OwnerBound:               false,
		RunningImagePath:         "/running/amq",
		RunningVersion:           "0.50.0",
		CurrentImagePath:         "/current/amq",
		CurrentVersion:           "0.50.1",
		ImageStatus:              "different",
		CanRepairInjectVia:       false,
		RepairReason:             "wake is live",
		RestartCapability:        "operator_only",
		OperatorTerminalRequired: true,
		NextAction:               "leave the live wake running",
	}

	output, err := captureEnvStdout(t, func() error {
		return writeJSON(os.Stdout, result)
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "{\n" +
		"  \"schema\": 1,\n" +
		"  \"agent\": \"codex\",\n" +
		"  \"root\": \"/queue\",\n" +
		"  \"can_start_here\": false,\n" +
		"  \"start_mode\": \"raw\",\n" +
		"  \"start_reason\": \"owning terminal required\",\n" +
		"  \"live_wake\": true,\n" +
		"  \"wake_status\": \"valid\",\n" +
		"  \"wake_pid\": 42,\n" +
		"  \"wake_mode\": \"raw\",\n" +
		"  \"owner_bound\": false,\n" +
		"  \"running_image_path\": \"/running/amq\",\n" +
		"  \"running_version\": \"0.50.0\",\n" +
		"  \"current_image_path\": \"/current/amq\",\n" +
		"  \"current_version\": \"0.50.1\",\n" +
		"  \"image_status\": \"different\",\n" +
		"  \"can_repair_inject_via\": false,\n" +
		"  \"repair_reason\": \"wake is live\",\n" +
		"  \"restart_capability\": \"operator_only\",\n" +
		"  \"operator_terminal_required\": true,\n" +
		"  \"next_action\": \"leave the live wake running\"\n" +
		"}\n"
	if output != want {
		t.Fatalf("v1 wake-check bytes changed:\n--- got ---\n%s--- want ---\n%s", output, want)
	}
}

func TestDoctorOpsJSONSchemaV1GoldenBytes(t *testing.T) {
	result := doctorResult{
		Checks: []doctorCheck{{Name: "Binary", Status: "ok", Message: "ok"}},
		Ops: &doctorOpsResult{
			Root: opsRoot{Path: "/queue", Source: "flag"},
			WakeLocks: []opsWakeLock{{
				Status:                   "missing",
				Agent:                    "codex",
				Root:                     "/queue",
				Lock:                     "/queue/.wake.lock",
				CanStartHere:             false,
				OperatorTerminalRequired: false,
			}},
		},
	}
	result.Summary.OK = 1

	output, err := captureEnvStdout(t, func() error {
		return writeJSON(os.Stdout, result)
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "{\n" +
		"  \"checks\": [\n" +
		"    {\n" +
		"      \"name\": \"Binary\",\n" +
		"      \"status\": \"ok\",\n" +
		"      \"message\": \"ok\"\n" +
		"    }\n" +
		"  ],\n" +
		"  \"summary\": {\n" +
		"    \"ok\": 1,\n" +
		"    \"warn\": 0,\n" +
		"    \"error\": 0\n" +
		"  },\n" +
		"  \"ops\": {\n" +
		"    \"root\": {\n" +
		"      \"path\": \"/queue\",\n" +
		"      \"source\": \"flag\"\n" +
		"    },\n" +
		"    \"agents\": null,\n" +
		"    \"wake_locks\": [\n" +
		"      {\n" +
		"        \"status\": \"missing\",\n" +
		"        \"agent\": \"codex\",\n" +
		"        \"root\": \"/queue\",\n" +
		"        \"lock\": \"/queue/.wake.lock\",\n" +
		"        \"can_start_here\": false,\n" +
		"        \"operator_terminal_required\": false\n" +
		"      }\n" +
		"    ],\n" +
		"    \"wake_quarantine\": {\n" +
		"      \"count\": 0,\n" +
		"      \"newest_age_seconds\": null\n" +
		"    },\n" +
		"    \"hints\": null\n" +
		"  }\n" +
		"}\n"
	if output != want {
		t.Fatalf("v1 doctor bytes changed:\n--- got ---\n%s--- want ---\n%s", output, want)
	}
}

func TestJSONSchemaOneMatchesDefaultBytes(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.50.1")

	wakeDefault, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	wakeExplicit, err := captureEnvStdout(t, func() error {
		return runWake([]string{
			"check", "--root", root, "--me", "codex", "--json", "--json-schema=1",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if wakeExplicit != wakeDefault {
		t.Fatalf("explicit wake schema 1 changed bytes:\n--- default ---\n%s--- explicit ---\n%s", wakeDefault, wakeExplicit)
	}

	doctorDefault, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--root", root, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	doctorExplicit, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--root", root, "--json", "--json-schema=1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if doctorExplicit != doctorDefault {
		t.Fatalf("explicit doctor schema 1 changed bytes:\n--- default ---\n%s--- explicit ---\n%s", doctorDefault, doctorExplicit)
	}
}

func TestWakeCheckJSONSchemaV2DirectStartUsesExplicitNullsAndArgv(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.50.1")

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{
			"check", "--root", root, "--me", "codex",
			"--json", "--json-schema=2",
		})
	})
	if err != nil {
		t.Fatalf("wake check v2: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check v2: %v\n%s", err, output)
	}
	if got["schema"] != float64(2) || got["agent"] != "codex" || got["root"] != canonicalWakeRoot(root) {
		t.Fatalf("identity = %#v", got)
	}
	platform := requireJSONObject(t, got, "platform")
	if platform["wake_supported"] != true || platform["reason_code"] != nil {
		t.Fatalf("platform = %#v", platform)
	}
	start := requireJSONObject(t, got, "start")
	if start["available"] != true || start["mode"] != wakeInjectModeRaw ||
		start["reason_code"] != nil || start["detail"] != nil {
		t.Fatalf("start = %#v", start)
	}
	wake := requireJSONObject(t, got, "wake")
	if wake["status"] != string(wakeLockMissing) || wake["live"] != false ||
		wake["pid"] != nil || wake["mode"] != nil || wake["owner_bound"] != false {
		t.Fatalf("wake = %#v", wake)
	}
	image := requireJSONObject(t, got, "image")
	running := requireJSONObject(t, image, "running")
	if running["path"] != nil || running["version"] != nil || image["status"] != wakeImageUnknown {
		t.Fatalf("image = %#v", image)
	}
	repair := requireJSONObject(t, got, "repair")
	if repair["inject_via_available"] != false || repair["reason_code"] != "no_wake_lock" || repair["detail"] != nil {
		t.Fatalf("repair = %#v", repair)
	}
	if got["restart_capability"] != wakeRestartAgentSafe {
		t.Fatalf("restart_capability = %#v", got["restart_capability"])
	}
	action := requireJSONObject(t, got, "action")
	if action["kind"] != "start_wake" || action["actor"] != "agent" ||
		action["reason_code"] != "wake_missing_start_available" || action["terminal_required"] != false {
		t.Fatalf("action = %#v", action)
	}
	command := requireJSONObject(t, action, "command")
	program, ok := command["program"].(string)
	if !ok || !filepath.IsAbs(program) {
		t.Fatalf("action command program = %#v", command["program"])
	}
	wantArgs := []any{"wake", "--root", canonicalWakeRoot(root), "--me", "codex"}
	if !reflect.DeepEqual(command["args"], wantArgs) {
		t.Fatalf("action command args = %#v, want %#v", command["args"], wantArgs)
	}
}

func TestDoctorOpsJSONSchemaV2NestsSingleWakeDecision(t *testing.T) {
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
		Generation:   "doctor-v2-generation",
		ImagePath:    "/opt/homebrew/bin/amq",
		ImageVersion: "0.50.1",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID: pid, Running: true, StartToken: "12345", BootID: "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.50.1")

	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{
			"--root", root, "--ops", "--json", "--json-schema=2",
		})
	})
	if err != nil {
		t.Fatalf("doctor ops v2: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode doctor v2: %v\n%s", err, output)
	}
	ops := requireJSONObject(t, got, "ops")
	locks, ok := ops["wake_locks"].([]any)
	if !ok || len(locks) != 1 {
		t.Fatalf("wake_locks = %#v", ops["wake_locks"])
	}
	lock, ok := locks[0].(map[string]any)
	if !ok {
		t.Fatalf("wake lock = %#v", locks[0])
	}
	wakeCheck := requireJSONObject(t, lock, "wake_check")
	if wakeCheck["schema"] != float64(2) || wakeCheck["agent"] != "codex" ||
		wakeCheck["restart_capability"] != wakeRestartOperatorOnly {
		t.Fatalf("nested wake_check = %#v", wakeCheck)
	}
	for _, duplicate := range []string{
		"can_start_here", "start_mode", "start_reason", "running_image_path",
		"running_version", "current_image_path", "current_version", "image_status",
		"restart_capability", "operator_terminal_required", "next_action",
	} {
		if _, exists := lock[duplicate]; exists {
			t.Fatalf("doctor v2 wake lock duplicated %q: %#v", duplicate, lock)
		}
	}
}

func TestJSONSchemaRequiresJSONAndKnownVersion(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name string
		run  func() error
		want string
	}{
		{
			name: "wake check schema requires JSON",
			run: func() error {
				return runWake([]string{"check", "--root", root, "--me", "codex", "--json-schema=2"})
			},
			want: "--json-schema requires --json",
		},
		{
			name: "doctor schema requires JSON",
			run: func() error {
				return runDoctor([]string{"--root", root, "--json-schema=2"})
			},
			want: "--json-schema requires --json",
		},
		{
			name: "wake check rejects schema 3",
			run: func() error {
				return runWake([]string{"check", "--root", root, "--me", "codex", "--json", "--json-schema=3"})
			},
			want: "--json-schema must be 1 or 2",
		},
		{
			name: "doctor rejects schema 3",
			run: func() error {
				return runDoctor([]string{"--root", root, "--json", "--json-schema=3"})
			},
			want: "--json-schema must be 1 or 2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWakeCheckV2ClassifierActions(t *testing.T) {
	baseDecision := func() wakeCheckDecision {
		return wakeCheckDecision{
			Agent: "codex",
			Root:  "/queue with spaces",
			Platform: wakeCheckPlatformDecision{
				OS:            "darwin",
				WakeSupported: true,
			},
			Start: wakeCheckStartDecision{Available: true, Mode: wakeInjectModeRaw},
			Wake:  wakeCheckWakeDecision{Status: string(wakeLockMissing)},
			Image: wakeCheckImageDecision{Status: wakeImageUnknown},
		}
	}
	tests := []struct {
		name      string
		configure func(*wakeCheckDecision) (wakeLockInspection, *opsWakeLock)
		kind      string
		actor     string
		reason    string
		restart   string
		args      []string
		terminal  bool
	}{
		{
			name: "missing direct start",
			configure: func(*wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				return wakeLockInspection{}, nil
			},
			kind: wakeActionStartWake, actor: wakeActionActorAgent,
			reason: wakeReasonMissingStartAvailable, restart: wakeRestartAgentSafe,
			args: []string{"wake", "--root", "/queue with spaces", "--me", "codex"},
		},
		{
			name: "missing full strength unavailable",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Start.Available = false
				d.Start.Mode = wakeInjectModeNone
				return wakeLockInspection{}, nil
			},
			kind: wakeActionConfigureInjector, actor: wakeActionActorOperator,
			reason: wakeReasonFullStrengthUnavailable, restart: wakeRestartUnavailable,
		},
		{
			name: "missing owning terminal",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Start.Available = false
				return wakeLockInspection{}, nil
			},
			kind: wakeActionStartWake, actor: wakeActionActorOperator,
			reason: wakeReasonOwningTerminalRequired, restart: wakeRestartOperatorOnly,
			args:     []string{"wake", "--root", "/queue with spaces", "--me", "codex"},
			terminal: true,
		},
		{
			name: "stale inject-via repair",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Wake.Status = string(wakeLockStale)
				d.Wake.Mode = wakeCheckOptionalString(wakeTargetInjectVia)
				d.Repair.InjectViaAvailable = true
				return wakeLockInspection{Exists: true, Status: wakeLockStale}, &opsWakeLock{
					Repair: wakeRepairCommand(d.Root, d.Agent),
				}
			},
			kind: wakeActionRepairWake, actor: wakeActionActorAgent,
			reason: wakeReasonStaleRepairAvailable, restart: wakeRestartAgentSafe,
			args: []string{"wake", "repair", "--root", "/queue with spaces", "--me", "codex"},
		},
		{
			name: "live raw wake",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Wake.Status = string(wakeLockValid)
				d.Wake.Live = true
				d.Wake.Mode = wakeCheckOptionalString(wakeInjectModeRaw)
				return wakeLockInspection{Exists: true, Status: wakeLockValid}, nil
			},
			kind: wakeActionPreserveLiveWake, actor: wakeActionActorOperator,
			reason: wakeReasonLiveWakePreserve, restart: wakeRestartOperatorOnly,
			terminal: true,
		},
		{
			name: "live inject-via wake",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Wake.Status = string(wakeLockValid)
				d.Wake.Live = true
				d.Wake.Mode = wakeCheckOptionalString(wakeTargetInjectVia)
				return wakeLockInspection{Exists: true, Status: wakeLockValid}, nil
			},
			kind: wakeActionPreserveLiveWake, actor: wakeActionActorOperator,
			reason: wakeReasonLiveWakePreserve, restart: wakeRestartOperatorOnly,
		},
		{
			name: "stale owner-bound wake",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Wake.Status = string(wakeLockStale)
				d.Wake.OwnerBound = true
				return wakeLockInspection{Exists: true, Status: wakeLockStale}, nil
			},
			kind: wakeActionRecoverOwner, actor: wakeActionActorOperator,
			reason: wakeReasonOwnerRecoveryRequired, restart: wakeRestartUnavailable,
			args: []string{"wake", "recover-owner", "--root", "/queue with spaces", "--me", "codex"},
		},
		{
			name: "stale inject-via without repair",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Wake.Status = string(wakeLockStale)
				d.Wake.Mode = wakeCheckOptionalString(wakeTargetInjectVia)
				return wakeLockInspection{Exists: true, Status: wakeLockStale}, nil
			},
			kind: wakeActionConfigureInjector, actor: wakeActionActorOperator,
			reason: wakeReasonFullStrengthUnavailable, restart: wakeRestartUnavailable,
		},
		{
			name: "stale generic without full strength injector",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Start.Available = false
				d.Start.Mode = wakeInjectModeNone
				d.Wake.Status = string(wakeLockStale)
				d.Wake.Mode = wakeCheckOptionalString(wakeInjectModeRaw)
				return wakeLockInspection{Exists: true, Status: wakeLockStale}, nil
			},
			kind: wakeActionConfigureInjector, actor: wakeActionActorOperator,
			reason: wakeReasonFullStrengthUnavailable, restart: wakeRestartUnavailable,
		},
		{
			name: "stale generic manual cleanup",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Wake.Status = string(wakeLockStale)
				d.Wake.Mode = wakeCheckOptionalString(wakeInjectModeRaw)
				return wakeLockInspection{Exists: true, Status: wakeLockStale}, nil
			},
			kind: wakeActionManualStaleCleanup, actor: wakeActionActorOperator,
			reason: wakeReasonStaleManualCleanupRequired, restart: wakeRestartOperatorOnly,
			terminal: true,
		},
		{
			name: "creating wake",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Wake.Status = string(wakeLockCreating)
				return wakeLockInspection{Exists: true, Status: wakeLockCreating}, nil
			},
			kind: wakeActionWaitForStableState, actor: wakeActionActorAgent,
			reason: wakeReasonWakeStateCreating, restart: wakeRestartUnavailable,
			args: []string{"wake", "check", "--root", "/queue with spaces", "--me", "codex", "--json", "--json-schema=2"},
		},
		{
			name: "unverified wake",
			configure: func(d *wakeCheckDecision) (wakeLockInspection, *opsWakeLock) {
				d.Wake.Status = string(wakeLockUnverified)
				return wakeLockInspection{Exists: true, Status: wakeLockUnverified}, nil
			},
			kind: wakeActionInspectUnverified, actor: wakeActionActorOperator,
			reason: wakeReasonWakeStateUnverified, restart: wakeRestartUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := baseDecision()
			inspection, opsLock := test.configure(&decision)
			classifyWakeCheckRestart(&decision, inspection, opsLock)

			if decision.Action.Kind != test.kind || decision.Action.Actor != test.actor ||
				decision.Action.ReasonCode != test.reason ||
				decision.RestartCapability != test.restart ||
				decision.Action.TerminalRequired != test.terminal {
				t.Fatalf("decision = %#v", decision)
			}
			if decision.Action.Message == "" {
				t.Fatal("action message is empty")
			}
			if test.args == nil {
				if decision.Action.Command != nil {
					t.Fatalf("command = %#v, want null", decision.Action.Command)
				}
			} else {
				if decision.Action.Command == nil ||
					!filepath.IsAbs(decision.Action.Command.Program) ||
					!reflect.DeepEqual(decision.Action.Command.Args, test.args) {
					t.Fatalf("command = %#v, want args %#v", decision.Action.Command, test.args)
				}
			}

			v1 := renderWakeCheckV1(decision)
			v2 := renderWakeCheckV2(decision)
			if v1.RestartCapability != v2.RestartCapability ||
				v1.NextAction != v2.Action.Message {
				t.Fatalf("cross-schema mismatch: v1=%#v v2=%#v", v1, v2)
			}
		})
	}
}

func TestWakeCheckV2ObservationChangedIsRetryOnly(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.50.1")

	decision := unstableWakeCheckDecision(root, "codex", wakeLockInspection{})
	if decision.RestartCapability != wakeRestartUnavailable ||
		decision.Action.Kind != wakeActionRetryCheck ||
		decision.Action.Actor != wakeActionActorAgent ||
		decision.Action.ReasonCode != wakeReasonObservationChanged ||
		decision.Action.Command == nil ||
		!reflect.DeepEqual(decision.Action.Command.Args, []string{
			"wake", "check", "--root", canonicalWakeRoot(root), "--me", "codex",
			"--json", "--json-schema=2",
		}) {
		t.Fatalf("observation-changed decision = %#v", decision)
	}
	if decision.Reload.Status != wakeReloadUnavailable ||
		decision.Reload.ReasonCode != wakeReloadReasonObservationChanged {
		t.Fatalf("observation-changed reload = %#v", decision.Reload)
	}
	for _, mutating := range []string{wakeActionStartWake, wakeActionRepairWake, wakeActionRecoverOwner} {
		if decision.Action.Kind == mutating {
			t.Fatalf("observation change advertised mutating action %q", mutating)
		}
	}
}

func TestWakeCheckV2ReloadObservationChangeUsesExistingSnapshotRetry(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	lock := validWakeResumeLockForTest()
	lock.Root = canonicalWakeRoot(root)
	lock.Agent = "codex"
	lock.Generation = "first-resume-generation"
	writeWakeLockForTest(t, root, "codex", lock)
	inspections := 0
	var expectedAfterChange map[string]wakeCheckTreeEntry
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspections++
		if inspections == 2 {
			replacement := lock
			replacement.Generation = "replacement-resume-generation"
			writeWakeLockExactForTest(t, root, "codex", replacement)
			expectedAfterChange = snapshotWakeCheckTree(t, root)
		}
		return wakeProcessInfo{
			PID: pid, Running: true, StartToken: lock.ProcessStart, BootID: lock.BootID,
			Executable: lock.RunningImageEvidence.ExecutionPath,
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.50.1")

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{
			"check", "--root", root, "--me", "codex", "--json", "--json-schema=2",
		})
	})
	if err != nil {
		t.Fatalf("wake check v2: %v", err)
	}
	var got wakeCheckResultV2
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check v2: %v\n%s", err, output)
	}
	if got.Reload.Status != wakeReloadUnavailable ||
		got.Reload.ReasonCode != wakeReloadReasonObservationChanged ||
		got.RestartCapability != wakeRestartUnavailable ||
		got.Action.Kind != wakeActionRetryCheck {
		t.Fatalf("changing reload observation = %#v", got)
	}
	if expectedAfterChange == nil {
		t.Fatal("test did not inject the lock change")
	}
	assertWakeCheckTreeUnchanged(t, root, expectedAfterChange)
}

func TestWakeCheckV2ReloadClassificationIsStructuralAndNonExecutable(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	if err := writeWakePreparedFileInDir(
		fixture.agentDir,
		fixture.root,
		fixture.agent,
		fixture.lock,
	); err != nil {
		t.Fatal(err)
	}
	validStatus := wakeReloadReady
	validReason := wakeReloadReasonReady
	invalidAdvertisementReason := wakeReloadReasonAdvertisementInvalid
	validInspection := fixture.lock
	tests := []struct {
		name       string
		mutate     func(*wakeLockInspection)
		wantStatus string
		wantReason string
	}{
		{
			name: "not live",
			mutate: func(inspection *wakeLockInspection) {
				inspection.Process.Running = false
			},
			wantStatus: wakeReloadUnavailable,
			wantReason: wakeReloadReasonNotLive,
		},
		{
			name: "legacy not advertised",
			mutate: func(inspection *wakeLockInspection) {
				inspection.Lock.ResumeSchema = 0
				inspection.Lock.ResumeOwner = nil
				inspection.Lock.RunningImageEvidence = nil
			},
			wantStatus: wakeReloadUnavailable,
			wantReason: wakeReloadReasonNotAdvertised,
		},
		{
			name: "future schema",
			mutate: func(inspection *wakeLockInspection) {
				inspection.Lock.ResumeSchema = wakeResumeSchemaV2 + 1
			},
			wantStatus: wakeReloadUnavailable,
			wantReason: wakeReloadReasonSchemaUnsupported,
		},
		{
			name: "malformed advertisement",
			mutate: func(inspection *wakeLockInspection) {
				inspection.Lock.RunningImageEvidence = nil
			},
			wantStatus: wakeReloadUnavailable,
			wantReason: invalidAdvertisementReason,
		},
		{
			name: "repair lineage",
			mutate: func(inspection *wakeLockInspection) {
				inspection.Lock.SourceGeneration = "repaired-generation"
			},
			wantStatus: wakeReloadUnavailable,
			wantReason: invalidAdvertisementReason,
		},
		{
			name:       "valid advertisement",
			wantStatus: validStatus,
			wantReason: validReason,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := validInspection
			if test.mutate != nil {
				test.mutate(&inspection)
			}
			got := classifyWakeCheckReload(fixture.root, fixture.agent, inspection)
			if got.Status != test.wantStatus || got.ReasonCode != test.wantReason {
				t.Fatalf("reload decision = %#v, want status=%q reason=%q", got, test.wantStatus, test.wantReason)
			}
		})
	}

	stubWakeCheckRuntime(t, false, "0.50.1")
	decision := buildWakeCheckDecision(fixture.root, fixture.agent, validInspection, &opsWakeLock{}, false)
	if decision.Reload.Status != validStatus ||
		decision.Reload.ReasonCode != validReason ||
		decision.RestartCapability != wakeRestartAgentSafe ||
		decision.Action.Kind != wakeActionRestartWake ||
		decision.Action.Command == nil {
		t.Fatalf("ready reload is not executable: %#v", decision)
	}
	rendered := renderWakeCheckV2(decision)
	if rendered.Reload.Status != validStatus ||
		rendered.Reload.ReasonCode != validReason {
		t.Fatalf("advertised reload output = %#v", rendered.Reload)
	}
}

func TestWakeCheckV2RejectsResumeAdvertisementBoundOnlyToArtifactAgent(t *testing.T) {
	for _, artifactAgent := range []string{"", "claude"} {
		name := artifactAgent
		if name == "" {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			lock := validWakeResumeLockForTest()
			lock.Agent = artifactAgent
			lock.ControlSocket = wakeControlSocketPath(lock.Root, artifactAgent, lock.Generation)
			inspection := wakeLockInspection{
				Exists:            true,
				Status:            wakeLockValid,
				PID:               lock.PID,
				Lock:              lock,
				Process:           wakeProcessInfo{PID: lock.PID, Running: true},
				IdentityConfirmed: true,
			}

			decision := buildWakeCheckDecision(lock.Root, "codex", inspection, &opsWakeLock{}, false)
			if decision.Reload.Status != wakeReloadUnavailable ||
				decision.Reload.ReasonCode != wakeReloadReasonAdvertisementInvalid {
				t.Fatalf("artifact-bound reload decision = %#v, want invalid advertisement", decision.Reload)
			}
		})
	}
}

func TestDoctorOpsV2RejectsResumeAdvertisementBoundOnlyToArtifactAgent(t *testing.T) {
	for _, test := range []struct {
		name          string
		artifactAgent string
		wantReason    string
	}{
		{name: "omitted", wantReason: wakeReloadReasonAdvertisementInvalid},
		{name: "mismatched", artifactAgent: "claude", wantReason: wakeReloadReasonNotLive},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			lock := validWakeResumeLockForTest()
			lock.Root = canonicalWakeRoot(root)
			lock.Agent = test.artifactAgent
			lock.ControlSocket = wakeControlSocketPath(lock.Root, lock.Agent, lock.Generation)
			writeWakeLockExactForTest(t, root, "codex", lock)
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return wakeProcessInfo{
					PID: pid, Running: true, StartToken: lock.ProcessStart, BootID: lock.BootID,
					Executable: lock.RunningImageEvidence.ExecutionPath,
					Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
				}
			})

			locks, _ := checkWakeLocksWithHintsSchema(root, []string{"codex"}, false, wakeCheckSchemaV2)
			if len(locks) != 1 || locks[0].WakeCheckDecision == nil {
				t.Fatalf("doctor wake locks = %#v", locks)
			}
			reload := locks[0].WakeCheckDecision.Reload
			if reload.Status != wakeReloadUnavailable || reload.ReasonCode != test.wantReason {
				t.Fatalf("doctor artifact-bound reload decision = %#v, want reason %q", reload, test.wantReason)
			}
		})
	}
}

func TestDoctorOpsV2UsesTrustedRosterAgentInsteadOfOuterRecord(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	lock := validWakeResumeLockForTest()
	lock.Root = canonicalWakeRoot(root)
	lock.ControlSocket = wakeControlSocketPath(lock.Root, lock.Agent, lock.Generation)
	writeWakeLockExactForTest(t, root, "codex", lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID: pid, Running: true, StartToken: lock.ProcessStart, BootID: lock.BootID,
			Executable: lock.RunningImageEvidence.ExecutionPath,
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.50.1")
	ownerEnv, err := encodeWakeOwnerEnv(*lock.ResumeOwner)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWakeOwner, ownerEnv)

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockValid {
		t.Fatalf("initial inspection = %#v", inspection)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := writeWakePreparedFileInDir(agentDir, root, "codex", inspection); err != nil {
		t.Fatal(err)
	}
	opsLock := opsWakeLock{Agent: "claude"}
	decorateOpsWakeLockWithWakeCheck(root, "codex", &opsLock, inspection, false, true)
	if opsLock.Agent != "codex" || opsLock.WakeCheckDecision == nil {
		t.Fatalf("doctor trusted-agent snapshot = %#v", opsLock)
	}
	wantStatus := wakeReloadReady
	wantReason := wakeReloadReasonReady
	if reload := opsLock.WakeCheckDecision.Reload; reload.Status != wantStatus || reload.ReasonCode != wantReason {
		t.Fatalf("doctor trusted-agent reload = %#v, want status=%q reason=%q", reload, wantStatus, wantReason)
	}
}

func TestWakeCheckV2UnsupportedPlatformReloadIsUnavailable(t *testing.T) {
	decision := unsupportedWakeCheckDecision("/queue", "codex")
	if decision.Reload.Status != wakeReloadUnavailable ||
		decision.Reload.ReasonCode != wakeReloadReasonPlatformUnsupported {
		t.Fatalf("unsupported reload decision = %#v", decision.Reload)
	}
	reload := renderWakeCheckV2(decision).Reload
	if reload.Status != wakeReloadUnavailable ||
		reload.ReasonCode != wakeReloadReasonPlatformUnsupported {
		t.Fatalf("unsupported reload output = %#v", reload)
	}
}

func TestWakeCheckV2RetryActionIsReadOnly(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.50.1")
	decision := unstableWakeCheckDecision(root, "codex", wakeLockInspection{})
	if decision.Action.Command == nil {
		t.Fatal("retry action has no command")
	}
	before := snapshotWakeCheckTree(t, root)
	if _, err := captureEnvStdout(t, func() error {
		return runWake(decision.Action.Command.Args[1:])
	}); err != nil {
		t.Fatalf("execute retry action: %v", err)
	}
	assertWakeCheckTreeUnchanged(t, root, before)
}

func TestWakeCheckV2ActionProgramKeepsPublicExecutableAndLiteralArgv(t *testing.T) {
	dir := t.TempDir()
	cellar := filepath.Join(dir, "Cellar", "amq", "0.50.1", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(cellar), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cellar, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(dir, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(public), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cellar, public); err != nil {
		t.Fatal(err)
	}
	oldExecutable := wakeCheckExecutable
	wakeCheckExecutable = func() (string, error) { return public, nil }
	t.Cleanup(func() { wakeCheckExecutable = oldExecutable })

	root := "/queue with spaces/and ' quotes"
	command := wakeCheckActionCommand("wake", "--root", root, "--me", "codex")
	if command == nil || command.Program != public {
		t.Fatalf("program = %#v, want unresolved public path %q", command, public)
	}
	wantArgs := []string{"wake", "--root", root, "--me", "codex"}
	if !reflect.DeepEqual(command.Args, wantArgs) {
		t.Fatalf("args = %#v, want literal %#v", command.Args, wantArgs)
	}
}

func TestWakeCheckV2WithholdsAdviceWhenExecutableIdentityIsUnavailable(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.50.1")
	baselineV1, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		stub func() (string, error)
	}{
		{name: "executable lookup failed", stub: func() (string, error) {
			return "", errors.New("executable unavailable")
		}},
		{name: "executable path is relative", stub: func() (string, error) {
			return "bin/amq", nil
		}},
		{name: "executable path has leading whitespace", stub: func() (string, error) {
			return " " + filepath.Join(root, "bin", "amq"), nil
		}},
		{name: "executable path has trailing whitespace", stub: func() (string, error) {
			return filepath.Join(root, "bin", "amq") + " ", nil
		}},
		{name: "executable path is not clean", stub: func() (string, error) {
			return filepath.Join(root, "bin") + "/../amq", nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldExecutable := wakeCheckExecutable
			wakeCheckExecutable = test.stub
			t.Cleanup(func() { wakeCheckExecutable = oldExecutable })

			v1, err := captureEnvStdout(t, func() error {
				return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
			})
			if err != nil {
				t.Fatal(err)
			}
			if v1 != baselineV1 {
				t.Fatalf("v1 bytes changed when executable identity was unavailable:\n--- baseline ---\n%s--- got ---\n%s", baselineV1, v1)
			}

			output, err := captureEnvStdout(t, func() error {
				return runWake([]string{
					"check", "--root", root, "--me", "codex",
					"--json", "--json-schema=2",
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			var got wakeCheckResultV2
			if err := json.Unmarshal([]byte(output), &got); err != nil {
				t.Fatal(err)
			}
			if got.RestartCapability != wakeRestartUnavailable ||
				got.Action.Kind != wakeActionRetryCheck ||
				got.Action.Actor != wakeActionActorAgent ||
				got.Action.ReasonCode != wakeReasonExecutableUnavailable ||
				got.Action.Command != nil || got.Action.TerminalRequired {
				t.Fatalf("unavailable executable decision = %#v", got)
			}
		})
	}
}

func TestWakeCheckV2UnsupportedPlatformClassification(t *testing.T) {
	got := renderWakeCheckV2(unsupportedWakeCheckDecision("/queue", "codex"))
	if got.Schema != wakeCheckSchemaV2 || got.Platform.WakeSupported ||
		got.Platform.ReasonCode == nil || *got.Platform.ReasonCode != wakeReasonPlatformUnsupported ||
		got.RestartCapability != wakeRestartUnavailable ||
		got.Action.Kind != wakeActionUnsupported || got.Action.Actor != wakeActionActorNone ||
		got.Action.ReasonCode != wakeReasonPlatformUnsupported || got.Action.Command != nil {
		t.Fatalf("unsupported classification = %#v", got)
	}
}

func TestUnsupportedWakeDispatchRequiresExplicitV2OptIn(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "legacy default", args: []string{"--json"}},
		{name: "legacy explicit one", args: []string{"--json", "--json-schema=1"}},
		{name: "help stays legacy", args: []string{"--help"}},
		{name: "schema without JSON stays legacy", args: []string{"--json-schema=2"}},
		{name: "v2 equals", args: []string{"--json", "--json-schema=2"}, want: true},
		{name: "v2 separate", args: []string{"--json", "--json-schema", "2"}, want: true},
		{name: "single dash", args: []string{"-json", "-json-schema=2"}, want: true},
		{name: "bool one", args: []string{"--json=1", "--json-schema=2"}, want: true},
		{name: "bool t", args: []string{"--json=t", "--json-schema=2"}, want: true},
		{name: "bool uppercase", args: []string{"--json=TRUE", "--json-schema=2"}, want: true},
		{name: "last bool disables", args: []string{"--json", "--json=false", "--json-schema=2"}},
		{name: "last bool enables", args: []string{"--json=false", "--json=1", "--json-schema=2"}, want: true},
		{name: "last schema disables", args: []string{"--json", "--json-schema=2", "--json-schema=1"}},
		{name: "last schema enables", args: []string{"--json", "--json-schema=1", "--json-schema=2"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := wakeCheckV2OptInPresent(test.args); got != test.want {
				t.Fatalf("wakeCheckV2OptInPresent(%#v) = %t, want %t", test.args, got, test.want)
			}
		})
	}
}

func TestDoctorOpsV2UsesSameNestedDecisionAsWakeCheck(t *testing.T) {
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
		Generation:   "shared-v2-generation",
		ImagePath:    "/opt/homebrew/bin/amq",
		ImageVersion: "0.50.1",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID: pid, Running: true, StartToken: "12345", BootID: "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.50.1")
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	wakeCheck := renderWakeCheckV2(inspectWakeCheckDecision(root, "codex"))
	doctor := runOpsChecksWithSchema(root, "test", false, wakeCheckSchemaV2)
	if len(doctor.WakeLocks) != 1 || doctor.WakeLocks[0].WakeCheckDecision == nil {
		t.Fatalf("doctor wake locks = %#v", doctor.WakeLocks)
	}
	doctorWakeCheck := renderWakeCheckV2(*doctor.WakeLocks[0].WakeCheckDecision)
	if !reflect.DeepEqual(doctorWakeCheck, wakeCheck) {
		t.Fatalf("doctor and wake check decisions differ:\ndoctor=%#v\nwake=%#v", doctorWakeCheck, wakeCheck)
	}
}

func TestDoctorOpsV2UsesOneStableSnapshotAfterEarlierDoctorStateChanged(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	lock := wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "12345",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "doctor-first-generation",
	}
	writeWakeLockForTest(t, root, "codex", lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		startToken := "12345"
		if pid == 5252 {
			startToken = "54321"
		}
		return wakeProcessInfo{
			PID: pid, Running: true, StartToken: startToken, BootID: "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	stubWakeCheckRuntime(t, false, "0.50.1")

	initial := inspectWakeLock(root, "codex")
	if initial.Status != wakeLockValid {
		t.Fatalf("initial inspection = %#v", initial)
	}
	lock.PID = 5252
	lock.ProcessStart = "54321"
	lock.Generation = "doctor-replacement-generation"
	writeWakeLockExactForTest(t, root, "codex", lock)

	opsLock := opsWakeLock{
		Status: string(initial.Status),
		Agent:  "codex",
		Root:   initial.Root,
		Lock:   initial.LockPath,
		PID:    initial.PID,
	}
	decorateOpsWakeLockWithWakeCheck(root, "codex", &opsLock, initial, false, true)
	if opsLock.WakeCheckDecision == nil {
		t.Fatal("doctor v2 wake decision is missing")
	}
	decision := *opsLock.WakeCheckDecision
	if opsLock.PID != lock.PID || decision.Wake.PID == nil || *decision.Wake.PID != lock.PID ||
		opsLock.Status != "live-raw-orphan" || decision.Action.Kind != wakeActionPreserveLiveWake {
		t.Fatalf("mixed doctor snapshots: outer=%#v decision=%#v", opsLock, decision)
	}
}

func TestDoctorOpsV2DoesNotMixRemovedOutcomeWithReplacementSnapshot(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	removed := wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "12345",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "removed-generation",
	}
	writeWakeLockForTest(t, root, "codex", removed)
	replacement := removed
	replacement.PID = 5252
	replacement.ProcessStart = "54321"
	replacement.Generation = "replacement-generation"
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == replacement.PID {
			return wakeProcessInfo{
				PID: pid, Running: true, StartToken: replacement.ProcessStart,
				BootID: replacement.BootID, Executable: replacement.Executable,
				Args: []string{"amq", "wake", "--root", root, "--me", "codex"},
			}
		}
		return wakeProcessInfo{PID: pid, Running: false}
	})
	stubWakeCheckRuntime(t, false, "0.50.1")

	removedInspection := inspectWakeLock(root, "codex")
	if removedInspection.PID != removed.PID || removedInspection.Status != wakeLockStale {
		t.Fatalf("removed inspection = %#v", removedInspection)
	}
	writeWakeLockExactForTest(t, root, "codex", replacement)
	result := doctorResult{Ops: &doctorOpsResult{WakeLocks: []opsWakeLock{{
		Status:  "fixed",
		Agent:   "codex",
		Root:    removedInspection.Root,
		Lock:    removedInspection.LockPath,
		PID:     removedInspection.PID,
		Reason:  "removed stale wake lock",
		Removed: true,
		Mutation: &opsWakeMutation{
			Status:  "fixed",
			Reason:  "removed stale wake lock",
			Removed: true,
		},
	}}}}
	decorateOpsWakeLockWithWakeCheck(
		root,
		"codex",
		&result.Ops.WakeLocks[0],
		removedInspection,
		false,
		true,
	)

	rendered := renderDoctorResultV2(result)
	if len(rendered.Ops.WakeLocks) != 1 {
		t.Fatalf("rendered wake locks = %#v", rendered.Ops.WakeLocks)
	}
	got := rendered.Ops.WakeLocks[0]
	if got.Removed && got.PID == replacement.PID {
		t.Fatalf("removed outcome was combined with replacement snapshot: %#v", got)
	}
	if got.Status != "live-raw-orphan" || got.PID != replacement.PID || got.Mutation == nil ||
		got.Mutation.Status != "fixed" || !got.Mutation.Removed {
		t.Fatalf("replacement snapshot and removal outcome were not separated: %#v", got)
	}
}

func TestDoctorOpsV2PreservesFailedRemovalMutation(t *testing.T) {
	root := secureTempDirForTest(t)
	original := wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "12345",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "failed-removal-generation",
	}
	writeWakeLockExactForTest(t, root, "codex", original)
	replacement := original
	replacement.PID = 5252
	replacement.ProcessStart = "54321"
	replacement.Generation = "replacement-after-recheck"
	inspectCalls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspectCalls++
		if inspectCalls == 2 {
			writeWakeLockExactForTest(t, root, "codex", replacement)
		}
		return wakeProcessInfo{PID: pid, Running: false}
	})

	locks, _ := checkWakeLocksWithHintsSchema(root, []string{"codex"}, true, wakeCheckSchemaV2)
	if len(locks) != 1 {
		t.Fatalf("doctor wake locks = %#v", locks)
	}
	got := locks[0]
	if got.Status == "error" {
		t.Fatalf("failed mutation leaked into fresh snapshot: %#v", got)
	}
	if got.Mutation == nil || got.Mutation.Status != "error" || got.Mutation.Removed ||
		!strings.Contains(got.Mutation.Reason, "wake lock changed while cleaning stale lock") {
		t.Fatalf("failed removal mutation = %#v", got.Mutation)
	}
	rendered := renderDoctorResultV2(doctorResult{Ops: &doctorOpsResult{WakeLocks: locks}})
	renderedMutation := rendered.Ops.WakeLocks[0].Mutation
	if renderedMutation == nil || renderedMutation.Status != "error" || renderedMutation.Removed {
		t.Fatalf("rendered failed removal mutation = %#v", renderedMutation)
	}
	encoded, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"mutation":{"status":"error","reason":`) ||
		!strings.Contains(string(encoded), `"removed":false`) {
		t.Fatalf("rendered mutation bytes = %s", encoded)
	}
}

func TestDoctorOpsV2PreservesGenerationChangeMutation(t *testing.T) {
	root := secureTempDirForTest(t)
	original := wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "12345",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "initial-generation",
	}
	writeWakeLockExactForTest(t, root, "codex", original)
	replacement := original
	replacement.PID = 5252
	replacement.ProcessStart = "54321"
	replacement.Generation = "changed-before-fix"
	inspected := make(chan struct{}, 1)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		select {
		case inspected <- struct{}{}:
		default:
		}
		return wakeProcessInfo{PID: pid, Running: false}
	})

	guardEntered := make(chan struct{})
	releaseGuard := make(chan struct{})
	guardDone := make(chan error, 1)
	go func() {
		guardDone <- withWakeLifecycleGuard(root, "codex", func() error {
			close(guardEntered)
			<-releaseGuard
			return nil
		})
	}()
	<-guardEntered

	locksDone := make(chan []opsWakeLock, 1)
	go func() {
		locks, _ := checkWakeLocksWithHintsSchema(root, []string{"codex"}, true, wakeCheckSchemaV2)
		locksDone <- locks
	}()
	<-inspected
	writeWakeLockExactForTest(t, root, "codex", replacement)
	close(releaseGuard)
	if err := <-guardDone; err != nil {
		t.Fatalf("release lifecycle guard: %v", err)
	}

	locks := <-locksDone
	if len(locks) != 1 {
		t.Fatalf("doctor wake locks = %#v", locks)
	}
	got := locks[0]
	if got.Reason == "wake lock changed before fix" {
		t.Fatalf("guarded-skip mutation leaked into fresh snapshot: %#v", got)
	}
	if got.Mutation == nil || got.Mutation.Status != string(wakeLockStale) ||
		got.Mutation.Reason != "wake lock changed before fix" || got.Mutation.Removed {
		t.Fatalf("generation-change mutation = %#v", got.Mutation)
	}
}

func TestWakeCheckV2AdvertisedStartRevalidatesChangedWakeState(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.50.1")
	decision := inspectWakeCheckDecision(root, "codex")
	if decision.Action.Kind != wakeActionStartWake ||
		decision.Action.Actor != wakeActionActorAgent ||
		decision.Action.Command == nil {
		t.Fatalf("advertised start = %#v", decision)
	}

	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "12345",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "arrived-after-check",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID: pid, Running: true, StartToken: "12345", BootID: "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	loopCalled := false
	err = runWakeWithLoop(decision.Action.Command.Args[1:], func(wakeConfig) error {
		loopCalled = true
		return nil
	})
	if err == nil || loopCalled {
		t.Fatalf("changed-state start: err=%v loop_called=%t", err, loopCalled)
	}
	after, readErr := os.ReadFile(lockPath)
	if readErr != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("changed-state start altered wake lock: err=%v before=%q after=%q", readErr, before, after)
	}
}

func TestWakeCheckV2AdvertisedRepairRevalidatesChangedGeneration(t *testing.T) {
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
	lock := wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		Started:      "2026-07-31T01:00:00Z",
		ProcessStart: "12345",
		BootID:       wakeRepairTestBootID,
		Executable:   "amq",
		WakeMode:     wakeTargetInjectVia,
		TargetDigest: targetDigest,
		Generation:   "advertised-generation",
	}
	writeWakeLockForTest(t, root, "codex", lock)
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	writeWakeRepairFloorForTest(t, root, "codex", target, nil)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo { return wakeProcessInfo{PID: pid} })
	stubWakeCheckRuntime(t, false, "0.50.1")

	decision := inspectWakeCheckDecision(root, "codex")
	if decision.Action.Kind != wakeActionRepairWake ||
		decision.Action.Actor != wakeActionActorAgent ||
		decision.Action.Command == nil {
		t.Fatalf("advertised repair = %#v", decision)
	}
	lock.Generation = "replacement-generation"
	writeWakeLockExactForTest(t, root, "codex", lock)
	before := snapshotWakeCheckTree(t, root)

	err = runWake(decision.Action.Command.Args[1:])
	if err == nil {
		t.Fatal("repair unexpectedly accepted changed generation")
	}
	assertWakeCheckTreeUnchanged(t, root, before)
}

func requireJSONObject(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, object[key])
	}
	return value
}
