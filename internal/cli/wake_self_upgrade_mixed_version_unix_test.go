//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const wakeSelfUpgradeHistoricalTag = "v0.58.0"

func TestWakeSelfUpgradeRestartRecordIsCompatibleWithV0580Reader(t *testing.T) {
	fixture := newWakeRestartFixture(t)
	record := fixture.record
	record.Source = wakeRestartSourceSelf
	prior := record.Candidate
	prior.Inode++
	record.RefusedCandidates = []wakeSelfUpgradeRefusedCandidate{
		wakeSelfUpgradeRefusedCandidateFromEvidence(prior),
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}

	sourceRoot := extractWakeSelfUpgradeHistoricalV0580(t)

	generatedTest := historicalWakeSelfUpgradeSourceCompatibilityTest(string(raw))
	generatedPath := filepath.Join(sourceRoot, "internal", "cli", "wake_self_upgrade_source_compat_test.go")
	if err := os.WriteFile(generatedPath, []byte(generatedTest), 0o600); err != nil {
		t.Fatal(err)
	}
	commandOutputForOwnerFence(
		t,
		sourceRoot,
		"go", "test", "./internal/cli",
		"-run", "TestHistoricalWakeRestartSelfSourceCompatibility",
		"-count=1",
	)
}

func extractWakeSelfUpgradeHistoricalV0580(t *testing.T) string {
	t.Helper()
	repoRootOutput, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		t.Fatalf("mixed-version git history unavailable: %v", err)
	}
	repoRoot := strings.TrimSpace(string(repoRootOutput))
	if output, err := exec.Command("git", "-C", repoRoot, "cat-file", "-e", wakeSelfUpgradeHistoricalTag+"^{commit}").CombinedOutput(); err != nil {
		t.Fatalf("historical tag %s unavailable: %v\n%s", wakeSelfUpgradeHistoricalTag, err, output)
	}

	buildRoot := t.TempDir()
	sourceRoot := filepath.Join(buildRoot, "source")
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(buildRoot, "historical.tar")
	commandOutputForOwnerFence(
		t,
		repoRoot,
		"git", "archive", "--format=tar", "--output="+archivePath, wakeSelfUpgradeHistoricalTag,
	)
	commandOutputForOwnerFence(t, "", "tar", "-xf", archivePath, "-C", sourceRoot)
	return sourceRoot
}

func historicalWakeSelfUpgradeSourceCompatibilityTest(record string) string {
	return `package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestHistoricalWakeRestartSelfSourceCompatibility(t *testing.T) {
	root := canonicalWakeRoot(t.TempDir())
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(` + strconv.Quote(record) + `), &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["root"] = root
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fsq.AgentBase(root, "codex"), wakeRestartFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer agentDir.Close()
	if err := agentDir.withFD(func(dirfd int) error {
		snapshot, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists || snapshot.Record.Status != wakeRestartPending {
			t.Fatalf("historical pending restart = %#v, exists=%v", snapshot.Record, exists)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("historical reader changed the compatible restart record")
	}
	quarantined, err := filepath.Glob(path + ".quarantined.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 0 {
		t.Fatalf("historical reader quarantined compatible restart record: %v", quarantined)
	}
}
`
}
