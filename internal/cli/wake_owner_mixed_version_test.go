//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const ownerFenceLegacyCommit = "e37067a91b4447c3ed99bf647b71e7ec9dbf3824"

func TestOwnerFencePreservesClaimAgainstExactE370Binary(t *testing.T) {
	repoRootCommand := exec.Command("git", "rev-parse", "--show-toplevel")
	repoRootOutput, err := repoRootCommand.CombinedOutput()
	if err != nil {
		t.Skipf("mixed-version git history unavailable: %v", err)
	}
	repoRoot := strings.TrimSpace(string(repoRootOutput))
	if output, err := exec.Command("git", "-C", repoRoot, "cat-file", "-e", ownerFenceLegacyCommit+"^{commit}").CombinedOutput(); err != nil {
		t.Skipf("mixed-version commit %s unavailable: %v\n%s", ownerFenceLegacyCommit, err, output)
	}

	buildRoot := t.TempDir()
	sourceRoot := filepath.Join(buildRoot, "source")
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(buildRoot, "legacy.tar")
	commandOutputForOwnerFence(
		t,
		repoRoot,
		"git",
		"archive",
		"--format=tar",
		"--output="+archivePath,
		ownerFenceLegacyCommit,
	)
	commandOutputForOwnerFence(t, "", "tar", "-xf", archivePath, "-C", sourceRoot)
	legacyBinary := filepath.Join(buildRoot, "amq-e37067a")
	commandOutputForOwnerFence(t, sourceRoot, "go", "build", "-o", legacyBinary, "./cmd/amq")

	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "mixed-version-owner-injector")
	owner, err := captureAuthoritativeCurrentWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	lock, err := newWakeLock(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		return publishAuthoritativeWakeClaimAt(dirfd, agentDir, root, "codex", target, lock)
	})
	_ = agentDir.Close()
	if err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	beforeBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeInfo.Mode().Perm() != wakeOwnerLockFileMode {
		t.Fatalf("owner fence mode = %o, want %o", beforeInfo.Mode().Perm(), wakeOwnerLockFileMode)
	}

	commands := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "acquire",
			args: []string{"wake", "--root", root, "--me", "codex", "--inject-via", injector},
			want: "unverified",
		},
		{
			name: "doctor fix",
			args: []string{"doctor", "--ops", "--fix-wake-locks"},
			want: "unverified",
		},
		{
			name: "repair",
			args: []string{"wake", "repair", "--root", root, "--me", "codex"},
			want: "refused",
		},
		{
			name: "retire",
			args: []string{"wake", "retire", "--root", root, "--me", "codex", "--inject-via", injector},
			want: "refused",
		},
	}
	for _, test := range commands {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, legacyBinary, test.args...)
			cmd.Env = ownerFenceCommandEnv(root)
			output, _ := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("legacy command timed out: %v\n%s", test.args, output)
			}
			if !strings.Contains(strings.ToLower(string(output)), test.want) {
				t.Fatalf("legacy command %v output missing %q:\n%s", test.args, test.want, output)
			}
			afterBytes, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatalf("legacy command %v removed owner fence: %v\n%s", test.args, err, output)
			}
			afterInfo, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterBytes, beforeBytes) || !os.SameFile(beforeInfo, afterInfo) {
				t.Fatalf("legacy command %v changed owner fence bytes or inode", test.args)
			}
		})
	}
}

func commandOutputForOwnerFence(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s %v timed out", name, args)
	}
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func ownerFenceCommandEnv(root string) []string {
	env := os.Environ()
	for _, name := range []string{
		envWakeOwner,
		envRoot,
		envRootID,
		envBaseRoot,
		envBaseRootID,
		envSession,
	} {
		env = unsetEnvVar(env, name)
	}
	env = setEnvVar(env, envRoot, root)
	return setEnvVar(env, "AMQ_NO_UPDATE_CHECK", "1")
}
