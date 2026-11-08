//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// Pro review of S1 found native placeholders were resolved as saved names
// before the managed wrapper could acquire and substitute their thread IDs.
func TestManagedCoopExecDoesNotForwardPrivateLaunchNonce(t *testing.T) {
	for _, provider := range []string{launch.ClaudeProvider, launch.CodexProvider} {
		t.Run(provider, func(t *testing.T) {
			project := t.TempDir()
			session := filepath.Join(project, "session")
			if err := fsq.EnsureRootDirs(session); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(session, provider); err != nil {
				t.Fatal(err)
			}
			identity, err := fsq.SnapshotDeliveryRoot(session)
			if err != nil {
				t.Fatal(err)
			}
			root, err := fsq.OpenDeliveryRoot(session, identity)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			amqExecutable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			nonce := "71717171-7171-4171-8171-717171717171"
			options := &launch.PrepareExecutionOptions{WakeMode: "disabled", AuditReason: "test"}
			lease, err := launch.AcquireLease(root, nonce)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.LockHandles(provider); err != nil {
				t.Fatal(err)
			}
			executable := "/usr/bin/true"
			argv := []string{executable}
			mode := launch.AdapterModeMint
			var dynamic []launch.DynamicArg
			preSpawn := provider == launch.CodexProvider
			version, conversation, backend, profile := "", nonce, "", ""
			if preSpawn {
				executable = filepath.Join(t.TempDir(), "codex")
				if err := os.WriteFile(executable, []byte("executable"), 0o700); err != nil {
					t.Fatal(err)
				}
				plan, err := launch.NewCodexAdapter(executable).PlanFreshDaemon(launch.PlanRequest{
					Handle: provider, LaunchNonce: nonce, ProjectRoot: project, SessionRoot: session,
					Cwd: project, AMQExecutable: amqExecutable, ResumePolicy: launch.ResumeEnabled,
				})
				if err != nil {
					t.Fatal(err)
				}
				argv, dynamic, mode = plan.Argv, plan.DynamicArgv, plan.AdapterMode
				version, conversation = launch.CodexDaemonVersion, ""
				backend, profile = launch.CommandsBackendName, launch.CommandsProfile().Identity()
			}
			ticket, err := launch.NewExecutionTicket(launch.ExecutionTicketRequest{
				Handle: provider, LaunchNonce: nonce, Mode: mode,
				Provider: provider, ConversationID: conversation, ProviderVersion: version,
				PreSpawnAcquire: preSpawn, Backend: backend, Profile: profile, DynamicArgv: dynamic,
				ProjectRoot: project, SessionRoot: session, Cwd: project,
				ProviderExecutable: executable, AMQExecutable: amqExecutable,
				TargetArgv: argv, Execution: options,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := launch.WriteExecutionTicket(root, lease, ticket); err != nil {
				t.Fatal(err)
			}
			if err := launch.WriteConversation(root, lease, launch.ConversationRecord{
				Version: launch.ConversationVersion, Handle: provider, State: launch.CapturePending, LaunchNonce: nonce,
			}); err != nil {
				t.Fatal(err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}

			oldCWD, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(project); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(oldCWD) })
			t.Setenv(launch.InternalLaunchNonceEnv, nonce)
			self := os.Getpid()
			owner := wakeOwner{PID: self, ProcessStart: "12345", BootID: "11111111-1111-1111-1111-111111111111", SessionID: 99}
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return wakeProcessInfo{PID: pid, Running: pid == self, StartToken: owner.ProcessStart, BootID: owner.BootID}
			})
			stubWakeProcessSID(t, func(int) (int, error) { return owner.SessionID, nil })

			sentinel := errors.New("exec sentinel")
			oldExec := coopExecProcess
			var execEnv, execArgv []string
			coopExecProcess = func(_ string, args []string, env []string) error {
				execArgv = append([]string(nil), args...)
				execEnv = append([]string(nil), env...)
				return sentinel
			}
			t.Cleanup(func() { coopExecProcess = oldExec })
			t.Setenv("TERM", "xterm-256color")
			err = runCoopExec(append([]string{
				"--root", session, "--me", provider, "--no-wake", "--managed-no-wake-reason", "test", executable,
			}, argv[1:]...))
			if !errors.Is(err, sentinel) {
				t.Fatalf("coop exec error = %v, want sentinel", err)
			}
			for _, entry := range execEnv {
				if strings.HasPrefix(entry, launch.InternalLaunchNonceEnv+"=") {
					t.Fatalf("provider environment contains private launch nonce: %q", entry)
				}
			}
			if len(execArgv) < 2 || execArgv[1] != "__launch-exec" {
				t.Fatalf("managed command did not reach ticket-owned wrapper: %q", execArgv)
			}
		})
	}
}
