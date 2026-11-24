//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func doctorWakeProjectFixture(t *testing.T) (base, session1, session2 string) {
	t.Helper()
	base = secureTempDirForTest(t)
	session1 = filepath.Join(base, "session1")
	session2 = filepath.Join(base, "session2")
	for _, root := range []string{base, session1, session2} {
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatalf("ensure root dirs: %v", err)
		}
	}
	if err := config.WriteConfig(filepath.Join(base, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"codex"},
	}, true); err != nil {
		t.Fatal(err)
	}
	// A lock from a previous boot of this machine whose PID the restart gave
	// to a live non-amq process: proven stale, exactly the restart field case
	// this change covers.
	stubCurrentWakeMachineID(t, testMachineUUID)
	foreignBoot := doctorWakeForeignBootIDForTest(t)
	for _, root := range []string{session1, session2} {
		writeWakeLockForTest(t, root, "codex", wakeLock{
			PID:        4242,
			TTY:        "ttys001",
			MachineID:  testMachineUUID,
			BootID:     foreignBoot,
			Executable: "amq",
		})
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, Executable: "go", Args: []string{"go", "test"}}
	})
	return base, session1, session2
}

// doctorWakeForeignBootIDForTest returns a boot uuid provably different from
// this boot, so the lock classifies stale rather than unverified. It skips
// when the running system exposes no comparable boot uuid.
func doctorWakeForeignBootIDForTest(t *testing.T) string {
	foreign := "ffffffff-0000-0000-0000-00000000000" + "1"
	if current := currentWakeBootID(); launch.IsBootUUID(current) &&
		launch.CompareBootID(foreign, current) != launch.BootIDMismatch {
		t.Skipf("no provably foreign boot uuid available (current %q)", current)
	}
	return foreign
}

func TestDoctorProjectWakeLockRowReportsSessionCounts(t *testing.T) {
	base, session1, session2 := doctorWakeProjectFixture(t)
	t.Setenv(envRoot, base)

	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--json"})
	})
	if err != nil {
		t.Fatalf("plain doctor: %v", err)
	}
	var result struct {
		Checks          []doctorCheck          `json:"checks"`
		WakeLockSummary *doctorWakeLockSummary `json:"wake_locks"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v\n%s", err, output)
	}
	row := findDoctorCheck(t, result.Checks, "Wake locks")
	if row.Status != "warn" {
		t.Fatalf("Wake locks status = %q (%q), want warn", row.Status, row.Message)
	}
	for _, want := range []string{"2 stale", "session1: 1", "session2: 1", "amq doctor --root "} {
		if !strings.Contains(row.Message, want) {
			t.Fatalf("Wake locks message %q missing %q", row.Message, want)
		}
	}
	if result.WakeLockSummary == nil || result.WakeLockSummary.Stale != 2 {
		t.Fatalf("wake_locks summary = %#v, want 2 stale", result.WakeLockSummary)
	}

	opsOutput, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--ops", "--root", base, "--json"})
	})
	if err != nil {
		t.Fatalf("ops doctor: %v", err)
	}
	var opsResult doctorResult
	if err := json.Unmarshal([]byte(opsOutput), &opsResult); err != nil {
		t.Fatal(err)
	}
	// --ops emits the same per-lock detail for every session as the plain row.
	opsRow := findDoctorCheck(t, opsResult.Checks, "Wake locks")
	if opsRow.Status != "warn" || !strings.Contains(opsRow.Message, "2 stale") {
		t.Fatalf("ops Wake locks = %q (%q), want the same 2 stale counts", opsRow.Status, opsRow.Message)
	}
	roots := map[string]bool{}
	for _, lock := range opsResult.Ops.WakeLocks {
		roots[filepath.Dir(filepath.Dir(filepath.Dir(lock.Lock)))] = true
	}
	if len(opsResult.Ops.WakeLocks) != 2 || !roots[session1] || !roots[session2] {
		t.Fatalf("ops wake lock roots = %#v, want the session1 and session2 rows", roots)
	}

	// Ops is the writer; a fresh summary recheck counts its results without
	// applying a second fix traversal.
	opsFixOutput, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--ops", "--root", base, "--fix-wake-locks", "--json"})
	})
	if err != nil {
		t.Fatalf("ops fix doctor: %v", err)
	}
	var opsFixResult doctorResult
	if err := json.Unmarshal([]byte(opsFixOutput), &opsFixResult); err != nil {
		t.Fatal(err)
	}
	if opsFixResult.WakeLockSummary == nil || opsFixResult.WakeLockSummary.Stale != 0 {
		t.Fatalf("summary after single ops fix = %#v, want no stale", opsFixResult.WakeLockSummary)
	}
	removed := 0
	for _, lock := range opsFixResult.Ops.WakeLocks {
		if lock.Removed {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("ops removed = %d, want both stale locks", removed)
	}

	// An explicit session --root scopes the row to that session alone.
	sessionOutput, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--json", "--root", filepath.Join(base, "session1")})
	})
	if err != nil {
		t.Fatalf("session-root doctor: %v", err)
	}
	var sessionResult doctorResult
	if err := json.Unmarshal([]byte(sessionOutput), &sessionResult); err != nil {
		t.Fatal(err)
	}
	sessionRow := findDoctorCheck(t, sessionResult.Checks, "Wake locks")
	if sessionRow.Status != "ok" || !strings.Contains(sessionRow.Message, "none stale") {
		t.Fatalf("session-root Wake locks = %q (%q), want only session1's stale lock", sessionRow.Status, sessionRow.Message)
	}
}

func TestDoctorFixWakeLocksRemovesProjectStaleLocksAndKeepsUnverified(t *testing.T) {
	base, session1, session2 := doctorWakeProjectFixture(t)
	t.Setenv(envRoot, base)

	// An unverified lock in session2 stays preserved by the fix.
	unverifiedPath := filepath.Join(fsq.AgentBase(session2, "legacy"), ".wake.lock")
	if err := os.MkdirAll(fsq.AgentBase(session2, "legacy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unverifiedPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--fix-wake-locks", "--json"})
	})
	if err != nil {
		t.Fatalf("doctor --fix-wake-locks: %v", err)
	}
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v\n%s", err, output)
	}
	for _, lockPath := range []string{
		filepath.Join(fsq.AgentBase(session1, "codex"), ".wake.lock"),
		filepath.Join(fsq.AgentBase(session2, "codex"), ".wake.lock"),
	} {
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("stale lock %s remains after fix: %v", lockPath, err)
		}
	}
	if _, err := os.Stat(unverifiedPath); err != nil {
		t.Fatalf("unverified lock was not preserved: %v", err)
	}
	if result.WakeLockSummary == nil || result.WakeLockSummary.Stale != 0 {
		t.Fatalf("wake lock summary after fix = %#v, want zero stale", result.WakeLockSummary)
	}

	// Structural mismatch is a stale verdict before process examination, not
	// proof that this lock is dead. The exact field-ruling boundary preserves it.
	mismatchPath := filepath.Join(fsq.AgentBase(session2, "foreign"), ".wake.lock")
	if err := os.MkdirAll(fsq.AgentBase(session2, "foreign"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, session2, "foreign", wakeLock{PID: 4242, Root: session1})
	if _, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--fix-wake-locks", "--json"})
	}); err != nil {
		t.Fatalf("doctor structural-mismatch fix: %v", err)
	}
	if _, err := os.Stat(mismatchPath); err != nil {
		t.Fatalf("root-mismatch lock was removed without process proof: %v", err)
	}
	structuralOutput, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--ops", "--fix-wake-locks", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var structuralResult doctorResult
	if err := json.Unmarshal([]byte(structuralOutput), &structuralResult); err != nil {
		t.Fatal(err)
	}
	for _, lock := range structuralResult.Ops.WakeLocks {
		if lock.Agent == "foreign" && lock.Fix != "" {
			t.Fatalf("structural stale lock offers an ineffective generic fix: %#v", lock)
		}
	}
	var structuralRow doctorCheck
	for _, check := range structuralResult.Checks {
		if check.Name == "Wake locks" {
			structuralRow = check
		}
	}
	if structuralRow.Message != "1 stale (session2: 1)" {
		t.Fatalf("retained structural summary = %q, want no generic fix", structuralRow.Message)
	}
	if len(structuralResult.WakeLockSummary.Fixes) != 0 {
		t.Fatalf("retained structural summary offers fixes = %#v", structuralResult.WakeLockSummary.Fixes)
	}
}
