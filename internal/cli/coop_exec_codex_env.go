package cli

import (
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// codexIdentityOverrides returns the Codex -c overrides that put each AMQ
// identity variable present in env into the environment of Codex's shell
// tool. Codex applies shell_environment_policy.set after its inherit mode,
// default excludes and exclude list, so a set value reaches every tool
// command unless an include_only list leaves it out
// (protocol/src/shell_environment.rs populate_env). A variable absent from
// env stays absent.
func codexIdentityOverrides(env []string) []string {
	var overrides []string
	for _, key := range identityEnvKeys {
		prefix := key + "="
		for _, entry := range env {
			if value, ok := strings.CutPrefix(entry, prefix); ok {
				overrides = append(overrides, "-c", launch.CodexIdentityOverride(key, value))
				break
			}
		}
	}
	return overrides
}

// refuseCodexSeatArgs stops a Codex seat whose arguments would take its
// AMQ identity away before coop exec names, records or starts anything. An
// app-server named by --remote is one AMQ does not see: neither a recorded
// thread identity nor the overrides reach its tool commands. A
// configuration override of shell_environment_policy, its set table, or an
// AMQ identity variable in it would replace the identity coop exec selected.
func refuseCodexSeatArgs(binaryPath string, args []string) error {
	if launch.ProviderForExecutable(binaryPath) != launch.CodexProvider {
		return nil
	}
	if codexArgsHaveRemote(args) {
		return ActionRequiredError("this Codex seat would run on an app-server AMQ does not see (--remote), where its amq commands get no AMQ identity; drop --remote, or pass --me and --root in each amq command. Codex was not started")
	}
	for _, value := range codexConfigOverrides(args) {
		key, _, _ := strings.Cut(value, "=")
		if codexOverrideSetsIdentity(key) {
			return UsageError("-c %s would replace the AMQ identity coop exec gives this Codex seat; drop that override. Codex was not started", strings.TrimSpace(key))
		}
	}
	return nil
}

// codexOverrideSetsIdentity reports whether a Codex configuration key
// replaces the shell environment table or sets an AMQ identity variable in
// it. Codex merges these keys without regard to case.
func codexOverrideSetsIdentity(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "shell_environment_policy" || key == "shell_environment_policy.set" {
		return true
	}
	name, ok := strings.CutPrefix(key, "shell_environment_policy.set.")
	if !ok {
		return false
	}
	for _, identity := range identityEnvKeys {
		if strings.EqualFold(name, identity) {
			return true
		}
	}
	return false
}

// pinCodexShellIdentity gives a Codex seat's tool commands the identity in
// env. A seat whose thread identity was recorded keeps its args: its tool
// commands carry CODEX_THREAD_ID, from which amq adopts the record on either
// backend, and an override would take a daemon thread off the daemon. Every
// other seat gets the overrides first in args, before any prompt or --;
// refuseCodexSeatArgs already refused any later override of them. Any
// override other than a feature boolean makes the TUI run its own
// app-server, so the identity also reaches a seat that would have run on
// the daemon.
func pinCodexShellIdentity(binaryPath string, args, env []string, recorded bool) []string {
	if recorded || launch.ProviderForExecutable(binaryPath) != launch.CodexProvider {
		return args
	}
	return append(codexIdentityOverrides(env), args...)
}

// codexDaemonSeatEnv is the environment for a Codex seat whose thread
// identity was recorded. Such a seat runs on the shared app-server daemon,
// and a TUI that finds no daemon starts it with its own environment, which
// every later seat's tool commands and hooks then inherit. Its identity
// travels in the record, so it carries no AM_* or AMQ_* variable.
func codexDaemonSeatEnv(binaryPath string, env []string, recorded bool) []string {
	if !recorded || launch.ProviderForExecutable(binaryPath) != launch.CodexProvider {
		return env
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, "AM_") && !strings.HasPrefix(entry, "AMQ_") {
			out = append(out, entry)
		}
	}
	return out
}

// codexArgsHaveRemote reports whether args name a --remote app-server
// before the prompt.
func codexArgsHaveRemote(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--remote" || strings.HasPrefix(arg, "--remote=") {
			return true
		}
	}
	return false
}
