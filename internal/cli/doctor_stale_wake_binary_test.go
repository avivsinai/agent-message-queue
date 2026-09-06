package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		!strings.Contains(hint.WakeBinary.Remedy, "restart") ||
		!strings.Contains(hint.WakeBinary.Remedy, "automatic self-upgrade state") {
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
