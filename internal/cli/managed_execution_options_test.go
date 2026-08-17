package cli

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestManagedExecutionOptionsCodecRoundTripAndHostileInput(t *testing.T) {
	want := launch.PrepareExecutionOptions{
		RequireWake: true, NoGitignore: true, WakeMode: "enabled",
		InjectorMode: "paste", InjectorVia: "/opt/amq/injector", InjectorArgs: []string{"--fixed", "value"},
		SymphonyEvents: []string{"after_create", "before_run"}, SymphonyWorkspaceKey: "workspace-7",
	}
	encoded, err := encodeManagedExecutionOptions(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeManagedExecutionOptions(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}

	unknown := base64.RawURLEncoding.EncodeToString([]byte(`{"wake_mode":"enabled","arbitrary_hook":"/tmp/run"}`))
	if _, err := decodeManagedExecutionOptions(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("arbitrary hook decode error = %v", err)
	}
	invalid := want
	invalid.SymphonyEvents = []string{"unknown"}
	if _, err := encodeManagedExecutionOptions(invalid); err == nil || !strings.Contains(err.Error(), "unknown symphony event") {
		t.Fatalf("unknown event error = %v", err)
	}
	argsWithoutVia := want
	argsWithoutVia.InjectorVia = ""
	if _, err := encodeManagedExecutionOptions(argsWithoutVia); err == nil || !strings.Contains(err.Error(), "injector args require via") {
		t.Fatalf("injector args without via error = %v", err)
	}
}

func TestManagedExecutionOptionsDisabledWakeRequiresReason(t *testing.T) {
	if err := validateManagedExecutionOptions(launch.PrepareExecutionOptions{WakeMode: "disabled"}); err == nil {
		t.Fatal("disabled wake without an audit reason succeeded")
	}
	if err := validateManagedExecutionOptions(launch.PrepareExecutionOptions{WakeMode: "disabled", AuditReason: "operator policy", RequireWake: true}); err == nil {
		t.Fatal("disabled wake with require_wake succeeded")
	}
}

func TestCoopExecRejectsManagedOptionsWithoutLaunchTicket(t *testing.T) {
	t.Setenv(launch.InternalLaunchNonceEnv, "")
	err := runCoopExec([]string{"--managed-symphony-event", "after_create", "true"})
	if err == nil || GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "require a trusted launch ticket") {
		t.Fatalf("untrusted managed option error = %v", err)
	}
}

func TestManagedCoopExecRequiresExplicitAbsoluteRoot(t *testing.T) {
	t.Setenv(launch.InternalLaunchNonceEnv, "11111111-1111-4111-8111-111111111111")
	for _, args := range [][]string{
		{"--managed-symphony-event", "after_create", "true"},
		{"--root", "relative/root", "--managed-symphony-event", "after_create", "true"},
	} {
		err := runCoopExec(args)
		if err == nil || GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), "explicit absolute --root") {
			t.Fatalf("managed exact-root error for %#v = %v", args, err)
		}
	}
}

func TestManagedCoopExecDoesNotProvisionMissingTicketRoot(t *testing.T) {
	t.Setenv(launch.InternalLaunchNonceEnv, "11111111-1111-4111-8111-111111111111")
	missing := filepath.Join(t.TempDir(), "missing")
	err := runCoopExec([]string{
		"--root", missing, "--no-wake", "--managed-no-wake-reason", "test",
		"--me", "codex", "true",
	})
	if err == nil || GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), "before root mutation") {
		t.Fatalf("missing managed root error = %v", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("managed refusal provisioned %s: %v", missing, statErr)
	}
}

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

func TestPrivateLaunchWrapperRejectsInvalidExecutionOptionsBeforeRootAccess(t *testing.T) {
	invalid := base64.RawURLEncoding.EncodeToString([]byte(`{"wake_mode":"enabled","arbitrary_hook":"/tmp/run"}`))
	err := runLaunchExec([]string{
		"--root", "/definitely/missing", "--handle", "codex",
		"--nonce", "11111111-1111-4111-8111-111111111111", "--target", "/bin/true",
		"--" + managedExecutionOptionsFlag, invalid, "--", "/bin/true",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid managed execution options") {
		t.Fatalf("private wrapper error = %v", err)
	}
}
