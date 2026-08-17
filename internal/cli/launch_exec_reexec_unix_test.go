//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestManagedLaunchReexecPreservesPIDAndTargetArgv(t *testing.T) {
	sentinel := errors.New("exec sentinel")
	old := coopExecProcess
	t.Cleanup(func() { coopExecProcess = old })
	var gotPath string
	var gotArgv, gotEnv []string
	coopExecProcess = func(path string, argv, env []string) error {
		gotPath = path
		gotArgv = slices.Clone(argv)
		gotEnv = slices.Clone(env)
		return sentinel
	}
	target := []string{"/opt/provider", "--resume", "conversation"}
	env := []string{"AM_ROOT=/queue", "TOKEN=value"}
	options := &launch.PrepareExecutionOptions{WakeMode: "enabled", RequireWake: true}
	err := reexecManagedLaunchWrapper("/queue", "codex", "11111111-1111-4111-8111-111111111111", target[0], target, env, options)
	if !errors.Is(err, sentinel) {
		t.Fatalf("reexec error = %v, want sentinel", err)
	}
	if gotPath == "" || len(gotArgv) < 12 || gotArgv[1] != "__launch-exec" {
		t.Fatalf("wrapper exec = path %q argv %#v", gotPath, gotArgv)
	}
	dash := slices.Index(gotArgv, "--")
	if dash < 0 || !slices.Equal(gotArgv[dash+1:], target) {
		t.Fatalf("wrapper target tail = %#v, want %#v", gotArgv[dash+1:], target)
	}
	optionsAt := slices.Index(gotArgv, "--"+managedExecutionOptionsFlag)
	if optionsAt < 0 || optionsAt+1 >= dash {
		t.Fatalf("private wrapper omitted execution options: %#v", gotArgv)
	}
	decoded, err := decodeManagedExecutionOptions(gotArgv[optionsAt+1])
	if err != nil || !slices.Equal(decoded.InjectorArgs, options.InjectorArgs) || decoded.RequireWake != options.RequireWake || decoded.WakeMode != options.WakeMode {
		t.Fatalf("private wrapper options = %#v, %v", decoded, err)
	}
	if !slices.Equal(gotEnv, env) {
		t.Fatalf("wrapper env = %#v, want %#v", gotEnv, env)
	}
}

func TestPrivateLaunchWrapperAcknowledgesThenRevertsFailedExec(t *testing.T) {
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
	provider := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(provider, []byte("provider"), 0o700); err != nil {
		t.Fatal(err)
	}
	injector := filepath.Join(t.TempDir(), "injector")
	if err := os.WriteFile(injector, []byte("injector"), 0o700); err != nil {
		t.Fatal(err)
	}
	options := &launch.PrepareExecutionOptions{
		WakeMode: "enabled", InjectorMode: "raw", InjectorVia: injector,
		InjectorArgs: []string{"--fixed", "ordered"},
	}
	amqExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nonce := "77777777-7777-4777-8777-777777777777"
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
		ProviderExecutable: provider, AMQExecutable: amqExecutable, TargetArgv: []string{provider}, Execution: options,
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
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	sentinel := errors.New("provider exec failed")
	oldExec := launchExecProcess
	launchExecProcess = func(string, []string, []string) error { return sentinel }
	t.Cleanup(func() { launchExecProcess = oldExec })
	encodedOptions, err := encodeManagedExecutionOptions(*options)
	if err != nil {
		t.Fatal(err)
	}
	err = runLaunchExec([]string{"--root", session, "--handle", "claude", "--nonce", nonce, "--target", provider, "--" + managedExecutionOptionsFlag, encodedOptions, "--", provider})
	if !errors.Is(err, sentinel) {
		t.Fatalf("wrapper error = %v, want provider exec failure", err)
	}
	record, err := launch.LoadConversation(root, "claude")
	if err != nil || record.State != launch.CapturePending || record.Reason != "spawn_failed" {
		t.Fatalf("reverted record = %#v, %v", record, err)
	}
}

func TestPrivateCursorLaunchWrapperExecsOnlyResolvedConversationSlot(t *testing.T) {
	project := t.TempDir()
	session := filepath.Join(project, "session")
	if err := fsq.EnsureRootDirs(session); err != nil {
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
	conversationID := "018f1f2a-bc34-71bd-9056-23838e27f859"
	provider := filepath.Join(t.TempDir(), "cursor-agent")
	script := "#!/bin/sh\n[ \"$1\" = create-chat ] || exit 91\nprintf '%s\\n' " + conversationID + "\n"
	if err := os.WriteFile(provider, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	amqExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nonce := "79797979-7979-4797-8797-797979797979"
	placeholder := "__AMQ_CONVERSATION_ID__"
	targetArgv := []string{provider, "--resume", placeholder}
	lease, err := launch.AcquireLease(root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("cursor"); err != nil {
		t.Fatal(err)
	}
	ticket, err := launch.NewExecutionTicket(launch.ExecutionTicketRequest{
		Handle: "cursor", LaunchNonce: nonce, Mode: launch.AdapterModeCapture, Provider: launch.CursorProvider,
		ProviderVersion: "2026.08.11-e8db854", PreSpawnAcquire: true,
		Backend: launch.CommandsBackendName, Profile: launch.CommandsProfile().Identity(),
		ProjectRoot: project, SessionRoot: session, Cwd: project,
		ProviderExecutable: provider, AMQExecutable: amqExecutable, TargetArgv: targetArgv,
		DynamicArgv: []launch.DynamicArg{{Index: 2, Kind: launch.DynamicArgConversationID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launch.WriteExecutionTicket(root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := launch.WriteConversation(root, lease, launch.ConversationRecord{
		Version: launch.ConversationVersion, Handle: "cursor", State: launch.CapturePending,
		ProviderVersion: "2026.08.11-e8db854", LaunchNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	sentinel := errors.New("provider exec failed")
	oldExec := launchExecProcess
	var gotArgv []string
	launchExecProcess = func(_ string, argv, _ []string) error {
		gotArgv = slices.Clone(argv)
		return sentinel
	}
	t.Cleanup(func() { launchExecProcess = oldExec })
	err = runLaunchExec([]string{"--root", session, "--handle", "cursor", "--nonce", nonce, "--target", provider, "--", provider, "--resume", placeholder})
	if !errors.Is(err, sentinel) {
		t.Fatalf("wrapper error = %v, want provider exec failure", err)
	}
	if !slices.Equal(gotArgv, []string{provider, "--resume", conversationID}) {
		t.Fatalf("provider argv = %#v", gotArgv)
	}
	loaded, err := launch.LoadExecutionTicket(root, "cursor")
	if err != nil || loaded.State != launch.ExecutionIdentityAcquired || loaded.ConversationID != conversationID {
		t.Fatalf("reverted Cursor ticket = %#v, %v", loaded, err)
	}
}
