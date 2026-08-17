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

func TestManagedCoopExecDoesNotForwardPrivateLaunchNonce(t *testing.T) {
	project := t.TempDir()
	session := filepath.Join(project, "session")
	if err := fsq.EnsureRootDirs(session); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(session, "claude"); err != nil {
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
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	ticket, err := launch.NewExecutionTicket(launch.ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: nonce, Mode: launch.AdapterModeMint,
		Provider: launch.ClaudeProvider, ConversationID: nonce,
		ProjectRoot: project, SessionRoot: session, Cwd: project,
		ProviderExecutable: "/usr/bin/true", AMQExecutable: amqExecutable,
		TargetArgv: []string{"/usr/bin/true"}, Execution: options,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launch.WriteExecutionTicket(root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := launch.WriteConversation(root, lease, launch.ConversationRecord{
		Version: launch.ConversationVersion, Handle: "claude", State: launch.CapturePending, LaunchNonce: nonce,
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
	var execEnv []string
	coopExecProcess = func(_ string, _ []string, env []string) error {
		execEnv = append([]string(nil), env...)
		return sentinel
	}
	t.Cleanup(func() { coopExecProcess = oldExec })
	err = runCoopExec([]string{
		"--root", session, "--me", "claude", "--no-wake", "--managed-no-wake-reason", "test", "/usr/bin/true",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("coop exec error = %v, want sentinel", err)
	}
	for _, entry := range execEnv {
		if strings.HasPrefix(entry, launch.InternalLaunchNonceEnv+"=") {
			t.Fatalf("provider environment contains private launch nonce: %q", entry)
		}
	}
}
