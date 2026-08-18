//go:build darwin

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// brewUpgradeENOENTBaseline is origin/main before amq-0ja treated a missing
// Darwin restart-stage parent as a hard error. Binary B built from this
// commit must reproduce Aviv's error class after dirA vanishes.
const brewUpgradeENOENTBaseline = "ba99bf1aa735f5ee72fd508a77ae20bf96fdf0c3"

const brewUpgradeParentENOENTClass = "open Darwin wake restart stage parent"

// Cellar-adjacent persisted paths (audit for amq-oo7; design only):
//
//	| Persist site | Derived from executable dir? | Survives that dir vanishing? |
//	| Darwin restart stage `<exeDir>/.<base>.amq-restart-<id>/<base>` | yes | no |
//	| `.wake.restart` `stage_path` / `bound_image.execution_path` | yes (absolute copy) | string survives in the mailbox; file does not |
//	| lock `image_path` / `running_image_evidence.execution_path` | yes when the wake still runs that image or a stage hardlink | string survives; open/stat fail |
//	| agent `.w.<generation>` control socket | no (mailbox) | yes |
//	| `--ready-file` | no (process temp) | yes |
//	| keepalive/hookinstall binaries | no (user config / PATH) | yes |
//
// Proposal implemented later by amq-ju3 (not this bead): stage Darwin restart
// images under the machine-local AMQ state dir (DefaultLaunchStateDir), at
// `<state>/wake-stages/<root-identity>/<handle>/<id>/` mode 0700 — not AM_ROOT
// (queue state, often inside a repo) and not TMPDIR (macOS purges). This E2E
// asserts reclaim after rm -rf of binary A's directory; ju3 flips
// assertStageAfterBinaryDirRemoval to require the stage file itself to survive.

func TestTwoBinaryBrewUpgradeStaleLockRemoval(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes two real amq binaries")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("pre-fix B reproduces parent ENOENT", func(t *testing.T) {
		fixture := newBrewUpgradeTwoBinaryFixture(t, repoRoot, repoRoot)
		unfixed := filepath.Join(t.TempDir(), "amq-unfixed")
		buildCommitAMQForTest(t, repoRoot, brewUpgradeENOENTBaseline, unfixed, "0.64.0-unfixed")
		stdout, stderr, exit := runBrewUpgradeStaleRemoval(t, unfixed, fixture.root)
		combined := string(stdout) + string(stderr)
		if !strings.Contains(combined, brewUpgradeParentENOENTClass) {
			t.Fatalf("pre-fix B exit=%d missing %q\nstdout=%s\nstderr=%s", exit, brewUpgradeParentENOENTClass, stdout, stderr)
		}
		if _, err := os.Lstat(fixture.lockPath); err != nil {
			t.Fatalf("pre-fix B removed the stale lock: %v", err)
		}
	})

	t.Run("fixed B reclaims after dirA vanishes", func(t *testing.T) {
		fixture := newBrewUpgradeTwoBinaryFixture(t, repoRoot, repoRoot)
		fixed := filepath.Join(t.TempDir(), "amq-fixed")
		buildRepoAMQForTest(t, repoRoot, fixed, "0.64.0-fixed")
		stdout, stderr, exit := runBrewUpgradeStaleRemoval(t, fixed, fixture.root)
		if exit != 0 {
			t.Fatalf("fixed B stale-lock removal exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		if _, err := os.Lstat(fixture.lockPath); !os.IsNotExist(err) {
			t.Fatalf("fixed B left the stale lock: %v", err)
		}
	})
}

type brewUpgradeTwoBinaryFixture struct {
	root      string
	lockPath  string
	binaryDir string
	stagePath string
}

func newBrewUpgradeTwoBinaryFixture(t *testing.T, repoRoot, source string) brewUpgradeTwoBinaryFixture {
	t.Helper()
	versionDir := filepath.Join(t.TempDir(), "Cellar", "amq", "0.63.3")
	binDir := filepath.Join(versionDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryA := filepath.Join(binDir, "amq")
	buildRepoAMQForTest(t, source, binaryA, "0.63.3")
	binaryA, err := filepath.EvalSymlinks(binaryA)
	if err != nil {
		t.Fatal(err)
	}
	versionDir, err = filepath.EvalSymlinks(versionDir)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	initializeWakeABIRoot(t, binaryA, repoRoot, root)
	root = canonicalWakeABIRoot(t, root)
	ready := filepath.Join(t.TempDir(), "wake-ready")
	cmd := exec.Command(binaryA,
		"wake", "--root", root, "--me", "codex",
		"--inject-mode", "none", "--interrupt=false", "--ready-file", ready,
	)
	cmd.Dir = repoRoot
	cmd.Env = wakeABICleanEnv()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wake with binary A: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if waitErr, exited := waitForWakeABIPath(ready, done); exited {
		t.Fatalf("wake A exited before readiness: %v", waitErr)
	} else if waitErr != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("wait for wake A: %v", waitErr)
	}

	inspection := inspectWakeLock(root, "codex")
	if !inspection.Exists || inspection.Lock.Generation == "" {
		stopWakeABIBinary(t, cmd, done)
		t.Fatalf("wake A lock = %#v", inspection)
	}
	stagePath := persistAdjacentRefusedRestart(t, binaryA, root, "codex", inspection.Lock.Generation)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL wake A: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wake A did not exit after SIGKILL")
	}
	stale := inspectWakeLock(root, "codex")
	if stale.Status != wakeLockStale {
		t.Fatalf("after stop, wake status = %s, want stale", stale.Status)
	}
	if err := os.RemoveAll(versionDir); err != nil {
		t.Fatal(err)
	}
	assertStageAfterBinaryDirRemoval(t, stagePath, versionDir)
	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatalf("stale lock missing after dirA removal: %v", err)
	}
	return brewUpgradeTwoBinaryFixture{
		root: root, lockPath: lockPath, binaryDir: versionDir, stagePath: stagePath,
	}
}

func persistAdjacentRefusedRestart(t *testing.T, binaryA, root, agent, generation string) string {
	t.Helper()
	candidate, err := captureWakeImageEvidence(binaryA, "0.63.3")
	if err != nil {
		t.Fatal(err)
	}
	requestID := "0123456789abcdef0123456789abcdef"
	stagePath, err := planWakeRestartStagePlatform(candidate, requestID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindWakeRestartCandidateAtPlatform(candidate, stagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	bound.file = nil
	record := wakeRestartRecord{
		Schema:     wakeRestartSchemaV1,
		Status:     wakeRestartRefused,
		Source:     wakeRestartSourceSelf,
		Reason:     "bind wake restart candidate: wake image changed while hashing",
		RequestID:  requestID,
		Generation: generation,
		Root:       canonicalWakeRoot(root),
		Agent:      agent,
		Owner:      validWakeResumeOwnerForTest(),
		Candidate:  candidate,
		StagePath:  stagePath,
	}
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := agentDir.withFD(func(dirfd int) error {
		return writeWakeRestartRecordAt(dirfd, agentDir, record)
	}); err != nil {
		t.Fatal(err)
	}
	return stagePath
}

func assertStageAfterBinaryDirRemoval(t *testing.T, stagePath, binaryDir string) {
	t.Helper()
	_, err := os.Lstat(stagePath)
	besideBinary := stagePath == binaryDir || strings.HasPrefix(stagePath, binaryDir+string(os.PathSeparator))
	if besideBinary {
		if !os.IsNotExist(err) {
			t.Fatalf("cellar-adjacent stage survived rm -rf of binary dir %s: %v", binaryDir, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("stable stage %s vanished with binary dir %s: %v", stagePath, binaryDir, err)
	}
}

func runBrewUpgradeStaleRemoval(t *testing.T, binaryB, root string) ([]byte, []byte, int) {
	t.Helper()
	return runRealAMQWithExit(t, binaryB, filepath.Dir(root), wakeABICleanEnv(),
		"doctor", "--root", root, "--ops", "--fix-wake-locks", "--json",
	)
}

func buildRepoAMQForTest(t *testing.T, sourceDir, dest, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", dest, "./cmd/amq")
	cmd.Dir = sourceDir
	cmd.Env = wakeABICleanEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", dest, err, output)
	}
}

func buildCommitAMQForTest(t *testing.T, repoRoot, commit, dest, version string) {
	t.Helper()
	archivePath := filepath.Join(t.TempDir(), "source.tar")
	archive := exec.Command("git", "archive", "--format=tar", "--output", archivePath, commit)
	archive.Dir = repoRoot
	if output, err := archive.CombinedOutput(); err != nil {
		t.Skipf("baseline commit %s unavailable: %v\n%s", commit, err, output)
	}
	sourceDir := t.TempDir()
	extract := exec.Command("tar", "-xf", archivePath, "-C", sourceDir)
	if output, err := extract.CombinedOutput(); err != nil {
		t.Fatalf("extract %s: %v\n%s", commit, err, output)
	}
	buildRepoAMQForTest(t, sourceDir, dest, version)
}
