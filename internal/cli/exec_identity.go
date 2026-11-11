package cli

import (
	"fmt"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/execidentity"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// recordExecIdentity records, before coop exec becomes a Claude Code
// process, the pid and start it will keep and the AMQ identity in env, so a
// reader can join Claude's per-process session file to the handle exactly.
// A failure only warns: the harness still starts.
func recordExecIdentity(binaryPath string, env []string) {
	if launch.ProviderForExecutable(binaryPath) != launch.ClaudeProvider {
		return
	}
	if err := publishExecIdentity(launch.ClaudeProvider, env); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "amq: warning: could not record the Claude session's AMQ identity: %v\n", err)
	}
}

func publishExecIdentity(harness string, env []string) error {
	id := launch.IdentityFromEnv(env)
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		return err
	}
	start, err := launch.ProcessStartUnix(holder)
	if err != nil {
		return err
	}
	return execidentity.Publish(execidentity.Record{
		Harness: harness, Holder: holder, StartUnix: start,
		Root: id.Root, BaseRoot: id.BaseRoot, Session: id.Session,
		Me: id.Me, RootID: id.RootID, BaseRootID: id.BaseRootID,
	})
}
