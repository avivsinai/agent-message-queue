//go:build darwin

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDoctorReportsInvalidWakeRestartRecordWithoutLockReadOnly(t *testing.T) {
	for _, test := range []struct {
		name       string
		raw        func(t *testing.T, root string) []byte
		wantReason string
	}{
		{
			name: "malformed",
			raw: func(_ *testing.T, _ string) []byte {
				return []byte(`{"schema":`)
			},
			wantReason: "parse wake restart request",
		},
		{
			name: "future schema",
			raw: func(t *testing.T, root string) []byte {
				return futureWakeRestartRecordRawForDoctorTest(t, root)
			},
			wantReason: "schema 3 unsupported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
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

			path := filepath.Join(root, "agents", "codex", wakeRestartFileName)
			raw := test.raw(t, root)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}

			result := runOpsChecksWithSchema(root, "test", false, wakeCheckSchemaV2)
			if len(result.WakeLocks) != 1 {
				t.Fatalf("doctor wake diagnostics = %#v, want one restart residue", result.WakeLocks)
			}
			got := result.WakeLocks[0]
			wantFix := doctorRootCommandForOS(root, "", "darwin", "--ops", "--fix-wake-locks")
			if got.Status != string(wakeLockMissing) || got.RestartStageStatus != "record-invalid" ||
				!strings.Contains(got.RestartStageReason, test.wantReason) || got.Fix != wantFix {
				t.Fatalf("doctor restart residue = %#v", got)
			}
			setDoctorIdentityPin(t, root)
			output, err := captureEnvStdout(t, func() error {
				return runDoctor([]string{"--root", root, "--ops"})
			})
			if err != nil {
				t.Fatalf("doctor --ops: %v", err)
			}
			if !strings.Contains(output, "restart_stage=record-invalid") ||
				!strings.Contains(output, test.wantReason) ||
				!strings.Contains(output, "fix="+wantFix) {
				t.Fatalf("doctor --ops omitted invalid restart residue: %q", output)
			}
			if _, err := os.Lstat(filepath.Join(root, "agents", "codex", ".wake.lock")); !os.IsNotExist(err) {
				t.Fatalf("doctor diagnosis created a wake lock: %v", err)
			}
			afterRaw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterRaw, raw) || !os.SameFile(before, after) {
				t.Fatal("doctor diagnosis mutated the invalid restart record")
			}
		})
	}
}

func TestDoctorDoesNotReportMissingAgentWithoutRestartResidue(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(
		filepath.Join(root, "meta", "config.json"),
		config.Config{Version: 1, Agents: []string{"codex"}},
		true,
	); err != nil {
		t.Fatal(err)
	}

	agentDir := fsq.AgentBase(root, "codex")
	if _, err := os.Lstat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("configured agent directory unexpectedly exists before doctor: %v", err)
	}
	result := runOpsChecksWithSchema(root, "test", false, wakeCheckSchemaV2)
	if len(result.WakeLocks) != 0 {
		t.Fatalf("missing agent without restart residue was reported: %#v", result.WakeLocks)
	}
	if _, err := os.Lstat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("read-only doctor created configured agent directory: %v", err)
	}
}

func TestDoctorFixQuarantinesInvalidWakeRestartRecordWithoutLock(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  func(t *testing.T, root string) []byte
	}{
		{name: "malformed", raw: func(_ *testing.T, _ string) []byte { return []byte(`{"schema":`) }},
		{name: "future schema", raw: futureWakeRestartRecordRawForDoctorTest},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
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

			agentDir := fsq.AgentBase(root, "codex")
			path := filepath.Join(agentDir, wakeRestartFileName)
			raw := test.raw(t, root)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}

			result := runOpsChecksWithSchema(root, "test", true, wakeCheckSchemaV1)
			if len(result.WakeLocks) != 1 || result.WakeLocks[0].Status != "fixed" ||
				!result.WakeLocks[0].Removed {
				t.Fatalf("doctor fix result = %#v", result.WakeLocks)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("canonical restart record survived fix: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(agentDir, ".wake.lock")); !os.IsNotExist(err) {
				t.Fatalf("doctor restart residue fix created a wake lock: %v", err)
			}
			assertExactWakeQuarantineForTest(
				t,
				agentDir,
				wakeRestartFileName+".quarantined.",
				raw,
				before,
			)
		})
	}
}

func TestDoctorFixReclaimsValidDarwinRestartStageWithoutLock(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
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

	record := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	evidence := bound.evidence
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	bound.file = nil
	record.Schema = wakeRestartSchemaV1
	record.Status = wakeRestartPending
	record.Root = canonicalWakeRoot(root)
	record.Agent = "codex"
	record.Generation = "abcdef0123456789abcdef0123456789"
	record.Owner = validWakeResumeOwnerForTest()
	record.BoundImage = &evidence

	agentDir, err := openWakeDirectory(fsq.AgentBase(root, "codex"), "wake agent directory")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := agentDir.withFD(func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, agentDir, record)
	}); err != nil {
		t.Fatal(err)
	}
	restartPath := filepath.Join(agentDir.path, wakeRestartFileName)
	raw, err := os.ReadFile(restartPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(restartPath)
	if err != nil {
		t.Fatal(err)
	}

	result := runOpsChecksWithSchema(root, "test", true, wakeCheckSchemaV1)
	if len(result.WakeLocks) != 1 || result.WakeLocks[0].Status != "fixed" ||
		!result.WakeLocks[0].Removed {
		t.Fatalf("doctor valid-stage fix result = %#v", result.WakeLocks)
	}
	if _, err := os.Lstat(record.StagePath); !os.IsNotExist(err) {
		t.Fatalf("owned Darwin restart stage survived doctor fix: %v", err)
	}
	assertExactWakeQuarantineForTest(
		t,
		agentDir.path,
		wakeRestartFileName+".quarantined.",
		raw,
		before,
	)
}

func TestDoctorFixPreservesRestartResidueWhenLockAppears(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentDir := fsq.AgentBase(root, "codex")
	restartPath := filepath.Join(agentDir, wakeRestartFileName)
	raw := []byte(`{"schema":`)
	if err := os.WriteFile(restartPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(restartPath)
	if err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{PID: os.Getpid()})

	err = fixWakeRestartResidueWithoutLock(root, "codex")
	if err == nil || !strings.Contains(err.Error(), "wake lock appeared") {
		t.Fatalf("fix with appeared wake lock error = %v", err)
	}
	afterRaw, err := os.ReadFile(restartPath)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(restartPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRaw, raw) || !os.SameFile(before, after) {
		t.Fatal("doctor fix mutated restart residue after a lock appeared")
	}
	quarantined, err := filepath.Glob(restartPath + ".quarantined.*")
	if err != nil || len(quarantined) != 0 {
		t.Fatalf("doctor fix quarantined restart residue after a lock appeared: %v, err=%v", quarantined, err)
	}
}

func futureWakeRestartRecordRawForDoctorTest(t *testing.T, root string) []byte {
	t.Helper()
	candidate, err := captureCurrentWakeImageEvidence()
	if err != nil {
		t.Fatal(err)
	}
	record := wakeRestartRecord{
		Schema:     wakeRestartSchemaV2 + 1,
		RequestID:  "0123456789abcdef0123456789abcdef",
		Status:     wakeRestartPending,
		Root:       canonicalWakeRoot(root),
		Agent:      "codex",
		Generation: "abcdef0123456789abcdef0123456789",
		Owner:      validWakeResumeOwnerForTest(),
		Candidate:  candidate,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func TestDoctorReportsRefusedSurvivingDarwinStageAsCleanupFailed(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	removeWakeRestartRecordForTest(t, fixture)
	record := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bound.close() }()

	record.Schema = wakeRestartSchemaV1
	record.Status = wakeRestartRefused
	record.Reason = "restart execution refused"
	record.Root = fixture.root
	record.Agent = fixture.agent
	record.Generation = fixture.lock.Lock.Generation
	record.Owner = fixture.owner
	evidence := bound.evidence
	record.BoundImage = &evidence
	if err := fixture.agentDir.withFD(func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, fixture.agentDir, record)
	}); err != nil {
		t.Fatal(err)
	}

	diagnostic := diagnoseWakeRestartStage(fixture.root, fixture.agent, fixture.lock)
	if diagnostic.Status != "cleanup-failed" || diagnostic.Path != record.StagePath ||
		diagnostic.Status == "pending" || diagnostic.Status == "handoff" {
		t.Fatalf("refused restart stage diagnostic = %#v", diagnostic)
	}
	locks, _ := checkWakeLocksWithHintsSchema(
		fixture.root,
		[]string{fixture.agent},
		false,
		wakeCheckSchemaV2,
	)
	if len(locks) != 1 || locks[0].RestartStageStatus != "cleanup-failed" ||
		locks[0].RestartStagePath != record.StagePath {
		t.Fatalf("doctor refused restart stage = %#v", locks)
	}
	if _, err := os.Lstat(record.StagePath); err != nil {
		t.Fatalf("doctor diagnosis mutated refused restart stage: %v", err)
	}
}
