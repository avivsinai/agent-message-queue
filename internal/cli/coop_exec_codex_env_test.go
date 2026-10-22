package cli

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// Bead agent-message-queue-vd4.1 (surface audit, codex-cli 0.163.0): under
// shell_environment_policy.inherit = "core" a coop exec Codex seat's shell
// tool saw no AM_*, its amq drain failed closed, and the model went looking
// in other projects until it read the fleet's mail. The seat's own identity
// now reaches the shell tool through -c overrides, unless a thread record
// already carries it.
func TestCodexSeatCarriesItsIdentityIntoTheShellTool(t *testing.T) {
	env := []string{"PATH=/bin", "AM_ROOT=/q/s1", "AM_BASE_ROOT=/q", "AM_SESSION=s1", "AM_ME=7",
		"AM_ROOT_ID=a\"b", "AM_BASE_ROOT_ID=c\\d"}
	args := []string{"-s", "read-only", "--", "fix it"}

	pinned, err := pinCodexShellIdentity("/usr/local/bin/codex", args, env, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-c", `shell_environment_policy.set.AM_ROOT="/q/s1"`,
		"-c", `shell_environment_policy.set.AM_BASE_ROOT="/q"`,
		"-c", `shell_environment_policy.set.AM_SESSION="s1"`,
		"-c", `shell_environment_policy.set.AM_ME="7"`,
		"-c", `shell_environment_policy.set.AM_ROOT_ID="a\"b"`,
		"-c", `shell_environment_policy.set.AM_BASE_ROOT_ID="c\\d"`,
		"-s", "read-only", "--", "fix it",
	}
	if !slices.Equal(pinned, want) {
		t.Fatalf("embedded seat args =\n%q\nwant\n%q", pinned, want)
	}

	resume := []string{"resume", "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a6"}
	if kept, err := pinCodexShellIdentity("codex", resume, env, true); err != nil || !slices.Equal(kept, resume) {
		t.Fatalf("recorded daemon thread args = %q, %v; want them unchanged", kept, err)
	}

	if _, err := pinCodexShellIdentity("codex", []string{"--remote", "ws://h:1"}, env, false); GetExitCode(err) != ExitActionRequired {
		t.Fatalf("--remote seat error = %v, want action required", err)
	}
}

// Bead agent-message-queue-vd4.1 (surface audit rollout): after a bare
// no-root refusal inside a Codex seat, the model searched other projects
// and sessions. Inside a Codex thread with no identity the refusal says not to.
func TestNoRootRefusalInsideCodexSaysNotToSearch(t *testing.T) {
	for _, key := range identityEnvKeys {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv("CODEX_THREAD_ID", "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a6")
	if err := noEligibleRootInGitError("/repo"); !strings.Contains(err.Error(), "do not search other projects or sessions") {
		t.Fatalf("refusal = %v", err)
	}
}
