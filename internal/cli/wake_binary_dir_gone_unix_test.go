//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeCheckAndDoctorClassifyVanishedBinaryDir(t *testing.T) {
	fixture := newVanishedBinaryStaleLockFixture(t)

	decision := inspectWakeCheckDecision(fixture.root, "codex")
	v1 := renderWakeCheckV1(decision)
	v2 := renderWakeCheckV2(decision)
	wantFix := doctorRootCommandForOS(fixture.root, "", runtime.GOOS, "--ops", "--fix-wake-locks")
	if v1.WakeStatus != string(wakeLockStale) ||
		v1.RestartCapability != wakeRestartAgentSafe ||
		v2.Action.ReasonCode != wakeReasonBinaryDirGone ||
		v1.NextAction != v2.Action.Message ||
		!strings.Contains(v1.NextAction, wakeBinaryDirGoneMessage) ||
		!strings.Contains(v1.NextAction, wantFix) {
		t.Fatalf("wake check vanished-binary = v1%#v v2%#v", v1, v2)
	}
	assertNoRawENOENT(t, v1.NextAction, v2.Action.Message, v2.Action.ReasonCode)

	result := runOpsChecksWithSchema(fixture.root, "test", false, wakeCheckSchemaV2)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("doctor wake locks = %#v", result.WakeLocks)
	}
	got := result.WakeLocks[0]
	if got.Status != string(wakeLockStale) ||
		got.Reason != wakeReasonBinaryDirGone ||
		got.WakeCheckDecision == nil ||
		got.WakeCheckDecision.Action.ReasonCode != wakeReasonBinaryDirGone ||
		!strings.Contains(got.WakeCheckDecision.Action.Message, wantFix) {
		t.Fatalf("doctor --ops vanished-binary = %#v decision=%#v", got, got.WakeCheckDecision)
	}
	assertNoRawENOENT(t, got.Reason, got.InspectionError, got.RestartStageReason, got.NextAction)

	v1ops := runOpsChecks(fixture.root, "test", false)
	if len(v1ops.WakeLocks) != 1 ||
		v1ops.WakeLocks[0].Reason != wakeReasonBinaryDirGone ||
		v1ops.WakeLocks[0].Fix != wantFix ||
		!strings.Contains(v1ops.WakeLocks[0].NextAction, wantFix) {
		t.Fatalf("doctor --ops v1 vanished-binary = %#v", v1ops.WakeLocks)
	}

	setDoctorIdentityPin(t, fixture.root)
	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--root", fixture.root, "--ops", "--json", "--json-schema=2"})
	})
	if err != nil {
		t.Fatalf("doctor --ops --json: %v", err)
	}
	assertNoRawENOENT(t, output)
	var encoded doctorResultV2
	if err := json.Unmarshal([]byte(output), &encoded); err != nil {
		t.Fatalf("decode doctor json: %v\n%s", err, output)
	}
	if encoded.Ops == nil || len(encoded.Ops.WakeLocks) != 1 {
		t.Fatalf("doctor json wake locks = %#v", encoded.Ops)
	}
	jsonLock := encoded.Ops.WakeLocks[0]
	if jsonLock.Reason != wakeReasonBinaryDirGone ||
		jsonLock.WakeCheck == nil ||
		jsonLock.WakeCheck.Action.ReasonCode != wakeReasonBinaryDirGone ||
		!strings.Contains(jsonLock.WakeCheck.Action.Message, wantFix) {
		t.Fatalf("doctor json vanished-binary = %#v", jsonLock)
	}

	fixed := runOpsChecks(fixture.root, "test", true)
	if len(fixed.WakeLocks) != 1 || fixed.WakeLocks[0].Status != "fixed" || !fixed.WakeLocks[0].Removed {
		t.Fatalf("doctor --fix-wake-locks = %#v", fixed.WakeLocks)
	}
	if _, err := os.Lstat(fixture.lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock survived fix: %v", err)
	}
}

func TestWakeCheckDoesNotClassifyMissingFileWhenParentRemains(t *testing.T) {
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
	parent := filepath.Join(t.TempDir(), "Cellar", "amq", "0.63.3", "bin")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(parent, "amq")
	if err := os.WriteFile(imagePath, []byte("amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
		ImagePath:  imagePath,
		WakeMode:   wakeInjectModeRaw,
		Generation: "missing-file-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})
	stubWakeCheckRuntime(t, true, "0.64.1")
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}

	decision := inspectWakeCheckDecision(root, "codex")
	v2 := renderWakeCheckV2(decision)
	if v2.Action.ReasonCode == wakeReasonBinaryDirGone {
		t.Fatalf("missing file with live parent classified as binary_dir_gone: %#v", v2)
	}
	if v2.Wake.Status != string(wakeLockStale) {
		t.Fatalf("wake status = %s, want stale", v2.Wake.Status)
	}
}

func TestUpgradePrintsPreviousBinaryWakeNote(t *testing.T) {
	baseRoot := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(baseRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(binary, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) {
		return binary, binary, nil
	}
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldFetchLatestTag := fetchLatestTagForUpgrade
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		return "v0.64.1", nil
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetchLatestTag })
	t.Setenv(envRoot, baseRoot)

	stdout, _, err := captureEnvOutput(t, func() error {
		return runUpgrade(nil, "v0.64.1")
	})
	if err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if !strings.Contains(stdout, upgradeLiveWakePreviousBinaryNote) {
		t.Fatalf("upgrade omitted previous-binary wake note:\n%s", stdout)
	}
}

func TestWakeRecordedPathParentGone(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "amq")
	if err := os.WriteFile(path, []byte("amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if wakeRecordedPathParentGone(path) {
		t.Fatal("present parent reported gone")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if wakeRecordedPathParentGone(path) {
		t.Fatal("missing file with present parent reported gone")
	}
	vanished := filepath.Join(t.TempDir(), "gone", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(vanished), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(vanished))); err != nil {
		t.Fatal(err)
	}
	if !wakeRecordedPathParentGone(vanished) {
		t.Fatal("vanished parent not detected")
	}
	replaced := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(replaced, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !wakeRecordedPathParentGone(filepath.Join(replaced, "amq")) {
		t.Fatal("non-directory parent not detected")
	}
	if wakeRecordedPathParentGone("amq") || wakeRecordedPathParentGone("") {
		t.Fatal("relative or empty path treated as vanished parent")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !wakeRecordedPathParentGone(filepath.Join(blocked, "bin", "amq")) {
		t.Fatal("ENOTDIR parent path not detected")
	}
}

func TestApplyWakeBinaryDirGoneOpsReasonEachClause(t *testing.T) {
	t.Run("nil lock", func(t *testing.T) {
		applyWakeBinaryDirGoneOpsReason(nil, wakeLockInspection{}, wakeRestartStageDiagnostic{})
	})
	t.Run("none", func(t *testing.T) {
		lock := &opsWakeLock{Reason: "stale"}
		applyWakeBinaryDirGoneOpsReason(lock, wakeLockInspection{}, wakeRestartStageDiagnostic{})
		if lock.Reason != "stale" {
			t.Fatalf("reason = %q, want unchanged", lock.Reason)
		}
	})
	t.Run("inspection", func(t *testing.T) {
		versionDir := filepath.Join(t.TempDir(), "Cellar", "amq", "0.63.3")
		imagePath := filepath.Join(versionDir, "bin", "amq")
		if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(imagePath, []byte("amq"), 0o700); err != nil {
			t.Fatal(err)
		}
		imagePath, err := filepath.EvalSymlinks(imagePath)
		if err != nil {
			t.Fatal(err)
		}
		versionDir, err = filepath.EvalSymlinks(versionDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(versionDir); err != nil {
			t.Fatal(err)
		}
		lock := &opsWakeLock{}
		applyWakeBinaryDirGoneOpsReason(lock, wakeLockInspection{Lock: wakeLock{ImagePath: imagePath}}, wakeRestartStageDiagnostic{})
		if lock.Reason != wakeReasonBinaryDirGone {
			t.Fatalf("inspection clause reason = %q", lock.Reason)
		}
	})
	t.Run("stage", func(t *testing.T) {
		lock := &opsWakeLock{}
		applyWakeBinaryDirGoneOpsReason(lock, wakeLockInspection{}, wakeRestartStageDiagnostic{Status: wakeRestartStageBinaryDirGone})
		if lock.Reason != wakeReasonBinaryDirGone {
			t.Fatalf("stage clause reason = %q", lock.Reason)
		}
	})
	t.Run("decision", func(t *testing.T) {
		lock := &opsWakeLock{
			WakeCheckDecision: &wakeCheckDecision{
				Action: wakeCheckActionDecision{ReasonCode: wakeReasonBinaryDirGone},
			},
		}
		applyWakeBinaryDirGoneOpsReason(lock, wakeLockInspection{}, wakeRestartStageDiagnostic{})
		if lock.Reason != wakeReasonBinaryDirGone {
			t.Fatalf("decision clause reason = %q", lock.Reason)
		}
	})
}

type vanishedBinaryStaleLockFixture struct {
	root     string
	lockPath string
}

func newVanishedBinaryStaleLockFixture(t *testing.T) vanishedBinaryStaleLockFixture {
	t.Helper()
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

	versionDir := filepath.Join(t.TempDir(), "Cellar", "amq", "0.63.3")
	binDir := filepath.Join(versionDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(binDir, "amq")
	if err := os.WriteFile(imagePath, []byte("amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	imagePath, err := filepath.EvalSymlinks(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	versionDir, err = filepath.EvalSymlinks(versionDir)
	if err != nil {
		t.Fatal(err)
	}

	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        4242,
		Executable: "/opt/homebrew/bin/amq",
		ImagePath:  imagePath,
		WakeMode:   wakeInjectModeRaw,
		Generation: "vanished-binary-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})
	stubWakeCheckRuntime(t, true, "0.64.1")
	if err := os.RemoveAll(versionDir); err != nil {
		t.Fatal(err)
	}
	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockStale {
		t.Fatalf("fixture status = %s, want stale", inspection.Status)
	}
	if !wakeInspectionBinaryDirGone(inspection) {
		t.Fatalf("fixture image parent still present: %#v", inspection.Lock)
	}
	return vanishedBinaryStaleLockFixture{root: root, lockPath: lockPath}
}

func assertNoRawENOENT(t *testing.T, parts ...string) {
	t.Helper()
	for _, part := range parts {
		lower := strings.ToLower(part)
		if strings.Contains(lower, "no such file") ||
			strings.Contains(lower, "enoent") ||
			strings.Contains(part, "open Darwin wake restart stage parent") {
			t.Fatalf("raw ENOENT leaked: %q", part)
		}
	}
}
