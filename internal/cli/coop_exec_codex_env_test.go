package cli

import (
	"os"
	"path/filepath"
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

	pinned := pinCodexShellIdentity("/usr/local/bin/codex", args, env, false)
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
	if kept := pinCodexShellIdentity("codex", resume, env, true); !slices.Equal(kept, resume) {
		t.Fatalf("recorded daemon thread args = %q; want them unchanged", kept)
	}
}

// #1176 review: a resume by id with --remote recorded a local identity and
// then started an app-server AMQ does not see, and a caller's
// -c shell_environment_policy.set.AM_ME won over the selected handle. coop
// exec refuses both before it names, records or starts anything.
func TestCodexSeatRefusesArgsThatDropItsIdentity(t *testing.T) {
	remote := []string{"resume", "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a6", "--remote", "ws://h:1"}
	if err := refuseCodexSeatArgs("codex", remote); GetExitCode(err) != ExitActionRequired {
		t.Fatalf("--remote resume error = %v, want action required", err)
	}
	other := []string{"-m", "gpt", "-c", `shell_environment_policy.set.AM_ME="other"`}
	if err := refuseCodexSeatArgs("codex", other); GetExitCode(err) != ExitUsage {
		t.Fatalf("identity override error = %v, want usage", err)
	}
	// #1176 review: include_only runs after set and dropped the identity.
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[shell_environment_policy]\ninclude_only = [\"PATH\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refuseCodexIncludePolicy("codex", nil); GetExitCode(err) != ExitUsage {
		t.Fatalf("include_only without AM_* error = %v, want usage", err)
	}
}

// #1176 review (live G2 run): a recorded seat's TUI started the shared
// Codex daemon with its own AM_ME and AM_ROOT, so every later seat on that
// daemon ran tool commands and hooks as the first seat.
func TestCodexDaemonSeatStartsWithoutAMQVariables(t *testing.T) {
	env := []string{"PATH=/bin", "AM_ROOT=/q/s1", "AM_ME=codex", "AM_ROOT_ID=x", "AMQ_WAKE_OWNER={}", "HOME=/h"}
	got := codexDaemonSeatEnv("/usr/local/bin/codex", env, true)
	if want := []string{"PATH=/bin", "HOME=/h"}; !slices.Equal(got, want) {
		t.Fatalf("daemon seat env = %q, want %q", got, want)
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
