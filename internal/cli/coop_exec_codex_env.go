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

// pinCodexShellIdentity gives a Codex seat's tool commands the identity in
// env. A seat whose thread identity was recorded keeps its args: its tool
// commands carry CODEX_THREAD_ID, from which amq adopts the record on either
// backend, and an override would take a daemon thread off the daemon. Every
// other seat gets the overrides first in args, before any prompt or --. Any
// override other than a feature boolean makes the TUI run its own
// app-server, so the identity also reaches a seat that would have run on
// the daemon. An app-server named by --remote is one AMQ does not see, so
// that seat does not start.
func pinCodexShellIdentity(binaryPath string, args, env []string, recorded bool) ([]string, error) {
	if recorded || launch.ProviderForExecutable(binaryPath) != launch.CodexProvider {
		return args, nil
	}
	if codexArgsHaveRemote(args) {
		return nil, ActionRequiredError("this Codex seat would run on an app-server AMQ does not see (--remote), where its amq commands get no AMQ identity; drop --remote, or pass --me and --root in each amq command. Codex was not started")
	}
	return append(codexIdentityOverrides(env), args...), nil
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
