package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func TestRunOpsChecks_BasicAgentStats(t *testing.T) {
	root := secureTempDirForTest(t)

	// Set up agent dirs and config
	agents := []string{"alice", "bob"}
	for _, a := range agents {
		if err := fsq.EnsureAgentDirs(root, a); err != nil {
			t.Fatalf("ensure agent dirs for %s: %v", a, err)
		}
	}
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	if err := config.WriteConfig(cfgPath, config.Config{
		Version: 1,
		Agents:  agents,
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Add unread messages for alice
	msg1 := filepath.Join(fsq.AgentInboxNew(root, "alice"), "msg1.md")
	if err := os.WriteFile(msg1, []byte("test message 1"), 0o600); err != nil {
		t.Fatalf("write msg1: %v", err)
	}
	msg2 := filepath.Join(fsq.AgentInboxNew(root, "alice"), "msg2.md")
	if err := os.WriteFile(msg2, []byte("test message 2"), 0o600); err != nil {
		t.Fatalf("write msg2: %v", err)
	}

	// Add DLQ message for bob
	dlqMsg := filepath.Join(fsq.AgentDLQNew(root, "bob"), "dlq1.md")
	if err := os.WriteFile(dlqMsg, []byte("dead letter"), 0o600); err != nil {
		t.Fatalf("write dlq: %v", err)
	}

	// Write presence for alice
	p := presence.New("alice", "busy", "working", time.Now())
	if err := presence.Write(root, p); err != nil {
		t.Fatalf("write presence: %v", err)
	}

	result := runOpsChecks(root, "test_source", false)

	// Check root
	if result.Root.Path != root {
		t.Errorf("root path = %q, want %q", result.Root.Path, root)
	}
	if result.Root.Source != "test_source" {
		t.Errorf("root source = %q, want %q", result.Root.Source, "test_source")
	}

	// Should have 2 agents
	if len(result.Agents) != 2 {
		t.Fatalf("agent count = %d, want 2", len(result.Agents))
	}

	// Find alice
	var alice, bob *opsAgent
	for i := range result.Agents {
		switch result.Agents[i].Handle {
		case "alice":
			alice = &result.Agents[i]
		case "bob":
			bob = &result.Agents[i]
		}
	}
	if alice == nil || bob == nil {
		t.Fatalf("expected alice and bob agents")
	}

	// Alice should have 2 unread messages
	if alice.UnreadCount != 2 {
		t.Errorf("alice unread = %d, want 2", alice.UnreadCount)
	}

	// Alice should have presence
	if alice.PresenceStatus != "busy" {
		t.Errorf("alice presence = %q, want %q", alice.PresenceStatus, "busy")
	}

	// Bob should have 1 DLQ
	if bob.DLQCount != 1 {
		t.Errorf("bob dlq = %d, want 1", bob.DLQCount)
	}

	// Bob should have unknown presence (no presence written)
	if bob.PresenceStatus != "unknown" {
		t.Errorf("bob presence = %q, want %q", bob.PresenceStatus, "unknown")
	}

}

func TestRunOpsChecksProjectsDeferredNotificationAttempt(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("ensure agent dirs: %v", err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"codex"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}
	writer := notificationattempt.NewWriter(root, "codex")
	lifecycle, err := writer.Begin([]string{"msg-doctor-deferred"}, "external")
	if err != nil {
		t.Fatalf("begin notification attempt: %v", err)
	}
	if err := writer.Transition(lifecycle, notificationattempt.StateDeferred, "provider busy"); err != nil {
		t.Fatalf("defer notification attempt: %v", err)
	}

	result := runOpsChecks(root, "test_source", false)
	if len(result.Agents) != 1 || len(result.Agents[0].NotificationAttempts) != 1 {
		t.Fatalf("ops agents = %#v, want one projected notification attempt", result.Agents)
	}
	attempt := result.Agents[0].NotificationAttempts[0]
	if attempt.State != notificationattempt.StateDeferred || !attempt.RetryPending {
		t.Fatalf("projected notification attempt = %#v, want deferred retry-pending", attempt)
	}
	foundHint := false
	for _, hint := range result.Hints {
		if hint.Code == "notification_deferred" {
			foundHint = true
			break
		}
	}
	if !foundHint {
		t.Fatalf("ops hints = %#v, want notification_deferred warning", result.Hints)
	}
}

func TestRunOpsChecks_RejectsInvalidConfiguredHandleBeforePaths(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"../escape", "good"},
	}, true); err != nil {
		t.Fatal(err)
	}
	result := runOpsChecks(root, "test", false)
	for _, agent := range result.Agents {
		if agent.Handle == "../escape" {
			t.Fatal("invalid configured handle was used")
		}
	}
	found := false
	for _, hint := range result.Hints {
		if hint.Code == "config_error" && strings.Contains(hint.Message, "../escape") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected config_error hint for invalid configured handle")
	}
}

func TestRunOpsChecks_OperatorGateReportsWithoutConfig(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, reservedHumanHandle); err != nil {
		t.Fatalf("ensure user dirs: %v", err)
	}
	gatePath := filepath.Join(fsq.AgentInboxNew(root, reservedHumanHandle), "gate.md")
	if err := os.WriteFile(gatePath, []byte("gate"), 0o600); err != nil {
		t.Fatalf("write gate: %v", err)
	}
	if err := os.Chtimes(gatePath, time.Now().Add(-44*time.Second), time.Now().Add(-44*time.Second)); err != nil {
		t.Fatalf("chtimes gate: %v", err)
	}

	result := runOpsChecks(root, "env", false)
	if result.OperatorGate == nil {
		t.Fatal("operator_gate is nil, want populated")
	}
	if result.OperatorGate.OpenCount != 1 {
		t.Fatalf("open_count = %d, want 1", result.OperatorGate.OpenCount)
	}
	if result.OperatorGate.OldestGateAgeSeconds < 43 || result.OperatorGate.OldestGateAgeSeconds > 45 {
		t.Fatalf("oldest_gate_age_seconds = %v, want about 44", result.OperatorGate.OldestGateAgeSeconds)
	}
}

func TestRunOpsChecks_ReportsStaleWakeLock(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("ensure alice dirs: %v", err)
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	if err := config.WriteConfig(cfgPath, config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	lockPath := writeWakeLockForTest(t, root, "alice", wakeLock{
		PID:        999999999,
		Executable: "/opt/homebrew/bin/amq",
	})

	result := runOpsChecks(root, "test_source", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) {
		t.Fatalf("status = %q, want stale", got.Status)
	}
	if got.Agent != "alice" {
		t.Fatalf("agent = %q, want alice", got.Agent)
	}
	if got.Reason != "pid not running" {
		t.Fatalf("reason = %q, want pid not running", got.Reason)
	}
	wantFix := doctorRootCommandForOS(root, "", runtime.GOOS, "--ops", "--fix-wake-locks")
	if got.Fix != wantFix {
		t.Fatalf("fix = %q, want %q", got.Fix, wantFix)
	}
	if strings.Contains(got.Fix, "--ignore-session-pin") {
		t.Fatalf("fix advice bypasses session pin: %q", got.Fix)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock should not be removed without fix flag: %v", err)
	}
}

func TestRunOpsChecks_FixesStaleWakeLock(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("ensure alice dirs: %v", err)
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	if err := config.WriteConfig(cfgPath, config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	lockPath := writeWakeLockForTest(t, root, "alice", wakeLock{
		PID:        999999999,
		Executable: "/opt/homebrew/bin/amq",
	})

	result := runOpsChecks(root, "test_source", true)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != "fixed" {
		t.Fatalf("status = %q, want fixed", got.Status)
	}
	if !got.Removed {
		t.Fatal("expected Removed=true")
	}
	if got.RepairAvailable || got.Repair != "" {
		t.Fatalf("repair should be cleared after fix: %#v", got)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock should be removed, stat err=%v", err)
	}
}

func establishDoctorWakeLifecycleGuardForTest(t *testing.T, root, agent string) {
	t.Helper()
	if err := withWakeLifecycleGuard(root, agent, func() error { return nil }); err != nil {
		t.Fatalf("establish wake lifecycle guard: %v", err)
	}
}
