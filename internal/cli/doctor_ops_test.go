package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
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

func TestRunOpsChecks_NoConfig(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}
	// No config.json written

	result := runOpsChecks(root, "env", false)

	// Should return config_error hint
	if len(result.Hints) == 0 {
		t.Fatal("expected hints for missing config")
	}
	found := false
	for _, h := range result.Hints {
		if h.Code == "config_error" && h.Status == "error" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected config_error hint, got: %+v", result.Hints)
	}
	// Should have no agents
	if len(result.Agents) != 0 {
		t.Errorf("expected no agents, got %d", len(result.Agents))
	}
}

func TestRunOpsChecks_ReportsAndFixesStaleWakeLockWithoutConfig(t *testing.T) {
	root := secureTempDirForTest(t)
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
	if got.Fix != fixWakeLocksCommand {
		t.Fatalf("fix = %q, want %q", got.Fix, fixWakeLocksCommand)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock should not be removed without fix flag: %v", err)
	}

	fixed := runOpsChecks(root, "test_source", true)
	if len(fixed.WakeLocks) != 1 {
		t.Fatalf("fixed wake lock count = %d, want 1", len(fixed.WakeLocks))
	}
	if fixed.WakeLocks[0].Status != "fixed" {
		t.Fatalf("fixed status = %q, want fixed", fixed.WakeLocks[0].Status)
	}
	if !fixed.WakeLocks[0].Removed {
		t.Fatal("expected Removed=true")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock should be removed with fix flag, stat err=%v", err)
	}

	foundConfigError := false
	for _, h := range result.Hints {
		if h.Code == "config_error" && h.Status == "error" {
			foundConfigError = true
			break
		}
	}
	if !foundConfigError {
		t.Fatalf("expected config_error hint, got: %+v", result.Hints)
	}
}

func TestRunOpsChecks_RootSourceThreaded(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	if err := config.WriteConfig(cfgPath, config.Config{
		Version: 1,
		Agents:  []string{},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	for _, src := range []string{"flag", "env", "project_amqrc", "global_amqrc"} {
		result := runOpsChecks(root, src, false)
		if result.Root.Source != src {
			t.Errorf("runOpsChecks(_, %q).Root.Source = %q", src, result.Root.Source)
		}
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
	if got.Fix != fixWakeLocksCommand {
		t.Fatalf("fix = %q, want %q", got.Fix, fixWakeLocksCommand)
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

func TestRunOpsChecks_ReportsWakeRepairAvailabilityWithoutSpawning(t *testing.T) {
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

	injector := filepath.Join(secureTempDirForTest(t), "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	target := mustNewWakeTargetForTest(t, root, "alice", injector, []string{"exec"})
	lockPath := writeWakeLockForTest(t, root, "alice", bindWakeLockToTarget(wakeLock{
		PID:        999999999,
		Executable: "/opt/homebrew/bin/amq",
	}, target))
	if err := writeWakeTarget(root, "alice", target); err != nil {
		t.Fatalf("write wake target: %v", err)
	}

	result := runOpsChecks(root, "test_source", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) {
		t.Fatalf("status = %q, want stale", got.Status)
	}
	if !got.TargetPresent {
		t.Fatalf("target_present = false, want true")
	}
	if got.TargetReason != "" {
		t.Fatalf("target_reason = %q, want empty", got.TargetReason)
	}
	if !got.RepairAvailable {
		t.Fatal("repair_available = false, want true")
	}
	if got.Repair != wakeRepairCommand(root, "alice") {
		t.Fatalf("repair = %q, want %q", got.Repair, wakeRepairCommand(root, "alice"))
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("doctor report should not remove lock: %v", err)
	}
}

func TestRunOpsChecks_StaleRawLockWithLeftoverTargetHasNoRepair(t *testing.T) {
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

	writeWakeLockForTest(t, root, "alice", wakeLock{
		PID:        999999999,
		Executable: "/opt/homebrew/bin/amq",
	})
	injector := filepath.Join(secureTempDirForTest(t), "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	if err := writeWakeTarget(root, "alice", mustNewWakeTargetForTest(t, root, "alice", injector, []string{"exec"})); err != nil {
		t.Fatalf("write wake target: %v", err)
	}

	result := runOpsChecks(root, "test_source", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) {
		t.Fatalf("status = %q, want stale", got.Status)
	}
	if got.RepairAvailable || got.Repair != "" {
		t.Fatalf("repair should not be available for unbound target: %#v", got)
	}
	if !strings.Contains(got.TargetReason, "not created for an inject-via repair target") {
		t.Fatalf("target_reason = %q, want unbound target reason", got.TargetReason)
	}
}

func TestRunOpsChecks_UnverifiedWakeLockHasTargetButNoRepair(t *testing.T) {
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

	const pid = 4242
	writeWakeLockForTest(t, root, "alice", wakeLock{
		PID:        pid,
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == pid {
			return wakeProcessInfo{
				PID:        gotPID,
				Running:    true,
				Executable: "/opt/homebrew/bin/amq",
			}
		}
		return wakeProcessInfo{PID: gotPID}
	})
	injector := filepath.Join(secureTempDirForTest(t), "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write injector: %v", err)
	}
	if err := writeWakeTarget(root, "alice", mustNewWakeTargetForTest(t, root, "alice", injector, nil)); err != nil {
		t.Fatalf("write wake target: %v", err)
	}

	result := runOpsChecks(root, "test_source", false)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(result.WakeLocks))
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockUnverified) {
		t.Fatalf("status = %q, want unverified", got.Status)
	}
	if !got.TargetPresent {
		t.Fatal("target_present = false, want true")
	}
	if got.RepairAvailable || got.Repair != "" {
		t.Fatalf("repair should not be available for unverified lock: %#v", got)
	}
}

func TestWakeRepairCommandQuotesRoot(t *testing.T) {
	got := wakeRepairCommand("/tmp/amq root/it's", "alice")
	want := "amq wake repair --root '/tmp/amq root/it'\"'\"'s' --me alice"
	if got != want {
		t.Fatalf("wakeRepairCommand = %q, want %q", got, want)
	}
}

func TestCheckGlobalRootHint_WithGlobalRC(t *testing.T) {
	// Create a fake home with ~/.amqrc
	fakeHome := t.TempDir()
	rcData, _ := json.Marshal(map[string]string{"root": ".agent-mail"})
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o644); err != nil {
		t.Fatalf("write ~/.amqrc: %v", err)
	}

	t.Setenv("HOME", fakeHome)
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	hints := checkGlobalRootHint()
	if len(hints) != 1 {
		t.Fatalf("expected 1 hint, got %d", len(hints))
	}
	if hints[0].Code != "global_root_configured" {
		t.Errorf("hint code = %q, want global_root_configured", hints[0].Code)
	}
	if hints[0].Status != "ok" {
		t.Errorf("hint status = %q, want ok", hints[0].Status)
	}
}

func TestCheckGlobalRootHint_WithGlobalEnv(t *testing.T) {
	// No ~/.amqrc but AMQ_GLOBAL_ROOT set
	fakeHome := t.TempDir()
	// No .amqrc in fakeHome

	t.Setenv("HOME", fakeHome)
	t.Setenv("AMQ_GLOBAL_ROOT", "/some/global/root")

	hints := checkGlobalRootHint()
	if len(hints) != 1 {
		t.Fatalf("expected 1 hint, got %d", len(hints))
	}
	if hints[0].Code != "global_root_configured" {
		t.Errorf("hint code = %q, want global_root_configured", hints[0].Code)
	}
}

func TestCheckGlobalRootHint_Missing(t *testing.T) {
	fakeHome := t.TempDir()
	// No .amqrc, no env var

	t.Setenv("HOME", fakeHome)
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	hints := checkGlobalRootHint()
	if len(hints) != 1 {
		t.Fatalf("expected 1 hint, got %d", len(hints))
	}
	if hints[0].Code != "global_root_missing" {
		t.Errorf("hint code = %q, want global_root_missing", hints[0].Code)
	}
	if hints[0].Status != "warn" {
		t.Errorf("hint status = %q, want warn", hints[0].Status)
	}
}

func TestCheckSymphonyHint_WithHooks(t *testing.T) {
	dir := t.TempDir()

	// Write WORKFLOW.md with AMQ hooks
	content := "# Workflow\n<!-- BEGIN AMQ MANAGED -->\nsome hooks\n<!-- END AMQ MANAGED -->\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write WORKFLOW.md: %v", err)
	}

	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	hints := checkSymphonyHint()
	if len(hints) != 1 {
		t.Fatalf("expected 1 hint, got %d", len(hints))
	}
	if hints[0].Code != "symphony_hooks_installed" {
		t.Errorf("hint code = %q, want symphony_hooks_installed", hints[0].Code)
	}
	if hints[0].Status != "ok" {
		t.Errorf("hint status = %q, want ok", hints[0].Status)
	}
}

func TestCheckSymphonyHint_WithoutHooks(t *testing.T) {
	dir := t.TempDir()

	// Write WORKFLOW.md without AMQ hooks
	content := "# Workflow\nSome regular workflow content\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write WORKFLOW.md: %v", err)
	}

	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	hints := checkSymphonyHint()
	if len(hints) != 1 {
		t.Fatalf("expected 1 hint, got %d", len(hints))
	}
	if hints[0].Code != "symphony_workflow_detected" {
		t.Errorf("hint code = %q, want symphony_workflow_detected", hints[0].Code)
	}
	if hints[0].Status != "warn" {
		t.Errorf("hint status = %q, want warn", hints[0].Status)
	}
}

func TestCheckSymphonyHint_NoWorkflow(t *testing.T) {
	dir := t.TempDir()
	// No WORKFLOW.md

	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	hints := checkSymphonyHint()
	if hints != nil {
		t.Errorf("expected nil hints for missing WORKFLOW.md, got %+v", hints)
	}
}
