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
