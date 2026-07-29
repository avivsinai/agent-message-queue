package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDoctorOpsReportsStructuredStaleWakeBinaryHintWithoutMutation(t *testing.T) {
	root := secureTempDirForTest(t)
	const (
		agent = "codex"
		pid   = 4242
	)
	lockPath := writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          pid,
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		Generation:   "stale-binary-generation",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        gotPID,
			Running:    true,
			StartToken: "12345",
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	stubWakeBinaryStaleness(t, func(inspection wakeLockInspection) (wakeBinaryStaleness, error) {
		if inspection.Agent != agent || inspection.PID != pid || !inspection.IdentityConfirmed {
			t.Fatalf("unexpected inspection: %#v", inspection)
		}
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	result := runOpsChecks(root, "test", true)
	hint, found := findOpsHint(result.Hints, "stale_wake_binary")
	if !found {
		t.Fatalf("stale_wake_binary hint missing: %#v", result.Hints)
	}
	if hint.Status != "warn" {
		t.Fatalf("status = %q, want warn", hint.Status)
	}
	if hint.Backlog != nil {
		t.Fatalf("stale wake hint populated backlog: %#v", hint.Backlog)
	}
	if hint.WakeBinary == nil {
		t.Fatal("wake_binary is nil")
	}
	if hint.WakeBinary.Agent != agent || hint.WakeBinary.PID != pid {
		t.Fatalf("wake_binary identity = %#v", hint.WakeBinary)
	}
	if hint.WakeBinary.Remedy == "" ||
		!strings.Contains(hint.WakeBinary.Remedy, "restart") {
		t.Fatalf("wake_binary remedy = %q", hint.WakeBinary.Remedy)
	}
	if !strings.Contains(hint.Message, "different amq executable") {
		t.Fatalf("exact-identity message = %q", hint.Message)
	}

	encoded, err := json.Marshal(hint)
	if err != nil {
		t.Fatalf("marshal hint: %v", err)
	}
	var wire struct {
		Code       string          `json:"code"`
		Status     string          `json:"status"`
		Backlog    json.RawMessage `json:"backlog"`
		WakeBinary *struct {
			Agent  string `json:"agent"`
			PID    int    `json:"pid"`
			Remedy string `json:"remedy"`
		} `json:"wake_binary"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal hint: %v", err)
	}
	if wire.Code != "stale_wake_binary" || wire.Status != "warn" ||
		wire.WakeBinary == nil || wire.WakeBinary.Agent != agent ||
		wire.WakeBinary.PID != pid || wire.WakeBinary.Remedy == "" {
		t.Fatalf("wire hint = %s", encoded)
	}
	if wire.Backlog != nil {
		t.Fatalf("wire hint unexpectedly includes backlog: %s", encoded)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("diagnostic removed wake lock: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(lockPath)); err != nil {
		t.Fatalf("diagnostic changed wake directory: %v", err)
	}
}

func TestDoctorOpsReportsLegacyFlagShapedStaleWakeWithoutAuthorizingFix(t *testing.T) {
	root := secureTempDirForTest(t)
	const (
		agent = "-legacy"
		pid   = 4242
	)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fsq.AgentBase(root, agent), 0o700); err != nil {
		t.Fatal(err)
	}
	lock := wakeLock{
		PID:          pid,
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		Generation:   "legacy-stale-binary-generation",
	}
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, agent), ".wake.lock")
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        gotPID,
			Running:    true,
			StartToken: "12345",
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	stubWakeBinaryStaleness(t, func(inspection wakeLockInspection) (wakeBinaryStaleness, error) {
		if inspection.Agent != agent || inspection.PID != pid || !inspection.IdentityConfirmed {
			t.Fatalf("unexpected inspection: %#v", inspection)
		}
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	for _, fix := range []bool{false, true} {
		result := runOpsChecks(root, "test", fix)
		hint, found := findOpsHint(result.Hints, "stale_wake_binary")
		if !found || hint.WakeBinary == nil || hint.WakeBinary.Agent != agent {
			t.Fatalf("fix=%v legacy stale-wake hint missing: %#v", fix, result.Hints)
		}
		if len(result.WakeLocks) != 1 || result.WakeLocks[0].Agent != agent {
			t.Fatalf("fix=%v legacy wake locks = %#v", fix, result.WakeLocks)
		}
		if result.WakeLocks[0].Fix != "" || result.WakeLocks[0].Repair != "" ||
			result.WakeLocks[0].RepairAvailable || result.WakeLocks[0].Removed {
			t.Fatalf("fix=%v authorized legacy mutation: %#v", fix, result.WakeLocks[0])
		}
		if len(result.Agents) != 0 {
			t.Fatalf("fix=%v exposed legacy handle as live agent: %#v", fix, result.Agents)
		}
		gotData, err := os.ReadFile(lockPath)
		if err != nil {
			t.Fatalf("fix=%v read legacy wake lock: %v", fix, err)
		}
		if !bytes.Equal(gotData, data) {
			t.Fatalf("fix=%v changed legacy wake lock bytes", fix)
		}
	}
}

func TestStaleWakeBinaryHintFailsClosedOnUnknownOrUnconfirmedEvidence(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	base := wakeLockInspection{
		Exists:            true,
		Status:            wakeLockValid,
		Root:              canonicalWakeRoot(root),
		Agent:             "codex",
		PID:               4242,
		IdentityConfirmed: true,
		Lock: wakeLock{
			PID:          4242,
			ProcessStart: "12345",
			Generation:   "generation",
		},
		Process: wakeProcessInfo{
			PID:        4242,
			Running:    true,
			StartToken: "12345",
		},
	}

	for _, tc := range []struct {
		name       string
		inspection wakeLockInspection
		probe      func(wakeLockInspection) (wakeBinaryStaleness, error)
	}{
		{
			name: "identity not confirmed",
			inspection: func() wakeLockInspection {
				got := base
				got.IdentityConfirmed = false
				return got
			}(),
			probe: func(wakeLockInspection) (wakeBinaryStaleness, error) {
				t.Fatal("probe called for unconfirmed wake")
				return wakeBinaryStaleness{}, nil
			},
		},
		{
			name:       "probe error",
			inspection: base,
			probe: func(wakeLockInspection) (wakeBinaryStaleness, error) {
				return wakeBinaryStaleness{}, errors.New("identity unavailable")
			},
		},
		{
			name:       "unknown comparison",
			inspection: base,
			probe: func(wakeLockInspection) (wakeBinaryStaleness, error) {
				return wakeBinaryStaleness{}, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubWakeBinaryStaleness(t, tc.probe)
			if hint, ok := checkStaleWakeBinaryHint(tc.inspection); ok {
				t.Fatalf("unexpected hint: %#v", hint)
			}
		})
	}
}

func TestStaleWakeBinaryHintDescribesDarwinTimestampEvidenceAsHeuristic(t *testing.T) {
	root := secureTempDirForTest(t)
	const agent = "codex"
	writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		Generation:   "darwin-heuristic-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "12345",
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonStartedMTime,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	result := runOpsChecks(root, "test", false)
	hint, found := findOpsHint(result.Hints, "stale_wake_binary")
	if !found {
		t.Fatalf("stale_wake_binary hint missing: %#v", result.Hints)
	}
	for _, want := range []string{"may be running an older amq binary", "timestamp heuristic"} {
		if !strings.Contains(hint.Message, want) {
			t.Fatalf("heuristic message missing %q: %q", want, hint.Message)
		}
	}
}

func TestStaleWakeBinaryHintSuppressesRacedProcessIdentity(t *testing.T) {
	root := secureTempDirForTest(t)
	const agent = "codex"
	writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		Generation:   "raced-process-generation",
	})
	inspectionCount := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspectionCount++
		start := "12345"
		if inspectionCount > 1 {
			start = "67890"
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: start,
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	result := runOpsChecks(root, "test", false)
	if hint, found := findOpsHint(result.Hints, "stale_wake_binary"); found {
		t.Fatalf("raced process produced stale binary hint: %#v", hint)
	}
}

func TestStaleWakeBinaryHintSuppressesChangedBinaryEvidence(t *testing.T) {
	root := secureTempDirForTest(t)
	const agent = "codex"
	writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		Generation:   "changed-binary-evidence-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "12345",
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	probes := 0
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		probes++
		return wakeBinaryStaleness{
			Stale:    probes == 1,
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	result := runOpsChecks(root, "test", false)
	if hint, found := findOpsHint(result.Hints, "stale_wake_binary"); found {
		t.Fatalf("changed binary evidence produced stale binary hint: %#v", hint)
	}
	if probes != 2 {
		t.Fatalf("binary staleness probes = %d, want 2", probes)
	}
}

func TestStaleWakeBinaryHintSuppressesChangedBinaryIdentityEvidence(t *testing.T) {
	root := secureTempDirForTest(t)
	const agent = "codex"
	writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		Generation:   "changed-binary-identity-evidence-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "12345",
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	probes := 0
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		probes++
		return wakeBinaryStaleness{
			Stale:  true,
			Method: wakeBinaryComparisonExactIdentity,
			Evidence: wakeBinaryEvidence{
				Available: true,
				Running:   wakeBinaryFileEvidence{Device: 1, Inode: uint64(probes)},
				Current:   wakeBinaryFileEvidence{Device: 2, Inode: uint64(probes)},
			},
		}, nil
	})

	result := runOpsChecks(root, "test", false)
	if hint, found := findOpsHint(result.Hints, "stale_wake_binary"); found {
		t.Fatalf("changed binary identity evidence produced stale binary hint: %#v", hint)
	}
	if probes != 2 {
		t.Fatalf("binary staleness probes = %d, want 2", probes)
	}
}

func TestStaleWakeBinaryHintSuppressesFinalProcessExecutableChange(t *testing.T) {
	root := secureTempDirForTest(t)
	const agent = "codex"
	writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		Generation:   "changed-final-executable-generation",
	})
	inspections := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspections++
		executable := "/opt/homebrew/bin/amq"
		if inspections == 3 {
			executable = "/usr/local/bin/amq"
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "12345",
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: executable,
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	probes := 0
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		probes++
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	result := runOpsChecks(root, "test", false)
	if hint, found := findOpsHint(result.Hints, "stale_wake_binary"); found {
		t.Fatalf("changed final executable produced stale binary hint: %#v", hint)
	}
	if probes != 2 || inspections != 3 {
		t.Fatalf("probes/inspections = %d/%d, want 2/3", probes, inspections)
	}
}

func TestStaleWakeBinaryHintSuppressesFinalProcessIdentityChange(t *testing.T) {
	root := secureTempDirForTest(t)
	const agent = "codex"
	writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(root),
		Agent:        agent,
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", agent},
		Generation:   "changed-final-identity-generation",
	})
	inspections := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspections++
		start := "12345"
		if inspections == 3 {
			start = "67890"
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: start,
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", agent},
		}
	})
	probes := 0
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		probes++
		return wakeBinaryStaleness{
			Stale:    true,
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	result := runOpsChecks(root, "test", false)
	if hint, found := findOpsHint(result.Hints, "stale_wake_binary"); found {
		t.Fatalf("changed final process identity produced stale binary hint: %#v", hint)
	}
	if probes != 2 || inspections != 3 {
		t.Fatalf("probes/inspections = %d/%d, want 2/3", probes, inspections)
	}
}

func stableWakeBinaryEvidenceForTest() wakeBinaryEvidence {
	return wakeBinaryEvidence{
		Available: true,
		Running:   wakeBinaryFileEvidence{Device: 1, Inode: 1},
		Current:   wakeBinaryFileEvidence{Device: 2, Inode: 2},
	}
}

func stubWakeBinaryStaleness(
	t *testing.T,
	fn func(wakeLockInspection) (wakeBinaryStaleness, error),
) {
	t.Helper()
	old := inspectWakeBinaryStaleness
	inspectWakeBinaryStaleness = fn
	t.Cleanup(func() { inspectWakeBinaryStaleness = old })
}
