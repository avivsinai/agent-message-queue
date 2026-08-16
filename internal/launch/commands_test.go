package launch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCommandsPublishedProfile(t *testing.T) {
	got := Commands{}.Detect()
	want := CommandsProfile()
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Profile.Identity() != "commands/any/v1" {
		t.Fatalf("profile identity = %q", got.Profile.Identity())
	}
	if !reflect.DeepEqual(got.Profile, want) {
		t.Fatalf("Detect profile = %#v, want %#v", got.Profile, want)
	}
	if !reflect.DeepEqual(got.Effective, want.Capabilities) {
		t.Fatalf("effective = %#v, want static maximum %#v", got.Effective, want.Capabilities)
	}
	if len(got.Degradations) != 0 {
		t.Fatalf("commands profile must not shrink at runtime: %#v", got.Degradations)
	}
}

func TestCommandsCreateEmitsCoopExecAndRoundTripsPlan(t *testing.T) {
	_, root := openTestRoot(t)
	plan := validPlan()
	result, err := Commands{}.Create(CreateRequest{
		Session: "feature-x", Plan: plan, AMQPath: "/opt/amq", Root: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeCommandsEmitted || !result.ActionRequired {
		t.Fatalf("Create = %#v, want commands_emitted + action_required", result)
	}
	if result.Outcome == OutcomeCreated || result.Outcome == OutcomeUnsupported {
		t.Fatalf("Create invented %s", result.Outcome)
	}
	decoded, err := DecodePlan(result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, plan) {
		t.Fatalf("DecodePlan round-trip mismatch\ngot %#v\nwant %#v", decoded, plan)
	}
	if len(result.Commands) != len(plan.Agents) {
		t.Fatalf("commands = %d, want %d", len(result.Commands), len(plan.Agents))
	}
	for i, agent := range plan.Agents {
		cmd := result.Commands[i]
		if cmd.Handle != agent.Handle || cmd.Cwd != agent.Cwd || cmd.LaunchNonce != agent.LaunchNonce {
			t.Fatalf("command[%d] metadata = %#v", i, cmd)
		}
		assertCoopExecGrammar(t, cmd.Argv, root.Base(), agent.Handle, agent.Argv)
		if cmd.Env[InternalLaunchNonceEnv] != agent.LaunchNonce {
			t.Fatalf("command[%d] managed launch nonce = %q, want %q", i, cmd.Env[InternalLaunchNonceEnv], agent.LaunchNonce)
		}
		if !strings.Contains(cmd.Line, "coop exec") || !strings.Contains(cmd.Line, agent.Handle) {
			t.Fatalf("command[%d] line missing coop exec: %s", i, cmd.Line)
		}
		if !strings.Contains(cmd.Line, agent.Argv[0]) {
			t.Fatalf("command[%d] line missing command positional: %s", i, cmd.Line)
		}
	}
	if _, err := LoadBinding(root); err == nil {
		t.Fatal("plan_only Create wrote a binding")
	}
}

func TestCommandsCreateCloseNeverInventSuccess(t *testing.T) {
	_, root := openTestRoot(t)
	created, err := Commands{}.Create(CreateRequest{Session: "collab", Plan: validPlan(), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if created.Outcome == OutcomeCreated {
		t.Fatal("Create invented managed success")
	}
	closed, err := Commands{}.Close(CloseRequest{Root: root, Binding: validBinding()})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Outcome != OutcomeUnsupported || closed.Reason != PlanOnlyCloseReason {
		t.Fatalf("Close = %#v, want unsupported %q", closed, PlanOnlyCloseReason)
	}
	if _, err := LoadBinding(root); err == nil {
		t.Fatal("Close wrote or required a binding")
	}
}

func TestCommandsInspectUnknownDoesNotMutate(t *testing.T) {
	_, root := openTestRoot(t)
	record := validBinding()
	lease := mustAcquireLease(t, root)
	if err := WriteBinding(root, lease, record); err != nil {
		t.Fatal(err)
	}
	got, err := Commands{}.Inspect(InspectRequest{Root: root, Binding: record})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != InspectUnknown || got.Evidence != PlanOnlyInspectEvidence || !got.ActionRequired {
		t.Fatalf("Inspect = %#v", got)
	}
	loaded, err := LoadBinding(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LaunchNonce != record.LaunchNonce {
		t.Fatalf("Inspect mutated binding: %#v", loaded)
	}
}

func TestCommandsCreateRejectsInvalidInput(t *testing.T) {
	_, err := Commands{}.Create(CreateRequest{Plan: validPlan()})
	if err == nil || !strings.Contains(err.Error(), "session is required") {
		t.Fatalf("empty session error = %v", err)
	}
	_, err = Commands{}.Create(CreateRequest{Session: "collab"})
	if err == nil {
		t.Fatal("invalid plan succeeded")
	}
	_, err = Commands{}.Create(CreateRequest{Session: "collab", Plan: validPlan()})
	if err == nil || !strings.Contains(err.Error(), "pinned session root is required") {
		t.Fatalf("missing pinned root error = %v", err)
	}
}

func TestCommandsCoopExecGrammarShape(t *testing.T) {
	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{
		{
			Handle: "claude", Argv: []string{"/usr/local/bin/claude", "--model", "opus"},
			Cwd: "/work/project", AdapterMode: AdapterModeUnsupported, ResumePolicy: ResumeDisabled,
		},
		{
			Handle: "codex", Argv: []string{"/usr/local/bin/codex"},
			Cwd: "/work/project", AdapterMode: AdapterModeUnsupported, ResumePolicy: ResumeDisabled,
		},
	}}
	_, root := openTestRoot(t)
	result, err := Commands{}.Create(CreateRequest{Session: "collab", Plan: plan, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(result.Commands))
	}
	assertCoopExecGrammar(t, result.Commands[0].Argv, root.Base(), "claude", plan.Agents[0].Argv)
	assertCoopExecGrammar(t, result.Commands[1].Argv, root.Base(), "codex", plan.Agents[1].Argv)
	if slices.Contains(result.Commands[1].Argv, "--") {
		t.Fatalf("lone executable must omit --: %#v", result.Commands[1].Argv)
	}
	if !strings.Contains(result.Commands[0].Line, "/usr/local/bin/claude --") {
		t.Fatalf("Line missing command positional before --: %s", result.Commands[0].Line)
	}
}

func TestCommandsCoopExecOldShapeFailsGrammar(t *testing.T) {
	old := []string{"amq", "coop", "exec", "--session", "collab", "--me", "claude", "--", "/usr/local/bin/claude", "--model", "opus"}
	if err := coopExecGrammarError(old, "/queue/collab", "claude", []string{"/usr/local/bin/claude", "--model", "opus"}); err == nil {
		t.Fatal("old shape (-- immediately after --me) passed grammar check")
	}
}

func TestTypedExecutionOptionsRoundTripAndSingleCoopExec(t *testing.T) {
	options := &PrepareExecutionOptions{
		RequireWake: true, NoGitignore: true, WakeMode: "enabled",
		InjectorMode: "raw", InjectorVia: "/opt/amq/injector", InjectorArgs: []string{"--fixed", "value with space"},
		SymphonyEvents: []string{"after_create", "before_run", "after_run", "before_remove"}, SymphonyWorkspaceKey: "workspace-7",
	}
	_, root := openTestRoot(t)
	plan := validPlan()
	plan.Agents = plan.Agents[1:]
	plan.Agents[0].Execution = options
	result, err := Commands{}.Create(CreateRequest{Session: "team", Plan: plan, AMQPath: "/opt/amq", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(result.Commands))
	}
	argv := result.Commands[0].Argv
	line := result.Commands[0].Line
	if strings.Count(line, "coop exec") != 1 {
		t.Fatalf("coop exec boundary count in %q = %d, want 1", line, strings.Count(line, "coop exec"))
	}
	for _, pair := range [][2]string{
		{"--root", root.Base()},
		{"--me", "codex"},
		{"--wake-inject-mode", "raw"},
		{"--wake-inject-via", "/opt/amq/injector"},
		{"--managed-symphony-workspace-key", "workspace-7"},
	} {
		at := indexOf(argv, pair[0])
		if at < 0 || at+1 >= len(argv) || argv[at+1] != pair[1] {
			t.Fatalf("%s transport in %#v, want %q", pair[0], argv, pair[1])
		}
	}
	if countArg(argv, "--wake-inject-arg") != 2 || countArg(argv, "--managed-symphony-event") != 4 {
		t.Fatalf("repeatable options were not lossless: %#v", argv)
	}
	if !slices.Contains(argv, "--require-wake") || !slices.Contains(argv, "--no-gitignore") || slices.Contains(argv, "--session") {
		t.Fatalf("typed option or exact-root transport mismatch: %#v", argv)
	}
}

func TestExplicitDefaultExecutionOptionsEmitNoFlags(t *testing.T) {
	_, root := openTestRoot(t)
	plan := validPlan()
	plan.Agents = plan.Agents[:1]
	plan.Agents[0].Execution = &PrepareExecutionOptions{WakeMode: "enabled"}
	if !reflect.DeepEqual(CanonicalExecutionOptions(plan.Agents[0].Execution), CanonicalExecutionOptions(nil)) {
		t.Fatal("explicit defaults and nil are not canonical equals")
	}
	result, err := Commands{}.Create(CreateRequest{Session: "team", Plan: plan, AMQPath: "/opt/amq", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	argv := result.Commands[0].Argv
	for _, flag := range []string{"--require-wake", "--no-gitignore", "--no-wake", "--wake-inject-mode", "--managed-symphony-event"} {
		if slices.Contains(argv, flag) {
			t.Fatalf("explicit default execution options emitted %s: %#v", flag, argv)
		}
	}
}

func TestExactRootAcrossSiblingWorktrees(t *testing.T) {
	worktrees := t.TempDir()
	mainCwd := filepath.Join(worktrees, "main")
	siblingCwd := filepath.Join(worktrees, "sibling")
	if err := os.Mkdir(mainCwd, 0o700); err != nil {
		t.Fatal(err)
	}
	runCommandsTestProcess(t, mainCwd, "git", "init", "-q")
	if err := os.WriteFile(filepath.Join(mainCwd, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommandsTestProcess(t, mainCwd, "git", "add", "README.md")
	runCommandsTestProcess(t, mainCwd, "git", "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-qm", "fixture")
	runCommandsTestProcess(t, mainCwd, "git", "worktree", "add", "-q", "-b", "sibling", siblingCwd)

	sessionRoot := filepath.Join(mainCwd, ".agent-mail", "collab")
	if err := fsq.EnsureRootDirs(sessionRoot); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"version":1,"agents":["claude","codex"]}`)
	if err := os.WriteFile(filepath.Join(sessionRoot, "meta", "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{"claude", "codex"} {
		if err := fsq.EnsureAgentDirs(sessionRoot, handle); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := fsq.SnapshotDeliveryRoot(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(sessionRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	moduleCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	moduleRoot := filepath.Clean(filepath.Join(moduleCwd, "..", ".."))
	binDir := t.TempDir()
	amqBinary := filepath.Join(binDir, "amq")
	build := exec.Command("go", "build", "-o", amqBinary, "./cmd/amq")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build amq fixture: %v\n%s", err, output)
	}
	provider := filepath.Join(binDir, "provider")
	providerScript := `#!/bin/sh
set -eu
case "$AM_ME" in
  claude) exec "$PROOF_AMQ" --no-update-check send --root "$AM_ROOT" --me claude --to codex --body "exact-root-message" ;;
  codex) exec "$PROOF_AMQ" --no-update-check drain --root "$AM_ROOT" --me codex --include-body ;;
  *) exit 64 ;;
esac
`
	if err := os.WriteFile(provider, []byte(providerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := "71717171-7171-4171-8171-717171717171"
	options := &PrepareExecutionOptions{WakeMode: "disabled", AuditReason: "hermetic exact-root proof"}
	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{
		{Handle: "claude", Argv: []string{provider}, EnvOverlay: map[string]string{"PROOF_AMQ": amqBinary}, Cwd: mainCwd, AdapterMode: AdapterModeCapture, ResumePolicy: ResumeFresh, LaunchNonce: nonce, Execution: clonePrepareExecutionOptions(options)},
		{Handle: "codex", Argv: []string{provider}, EnvOverlay: map[string]string{"PROOF_AMQ": amqBinary}, Cwd: siblingCwd, AdapterMode: AdapterModeCapture, ResumePolicy: ResumeFresh, LaunchNonce: nonce, Execution: clonePrepareExecutionOptions(options)},
	}}
	lease, err := AcquireLease(root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("claude", "codex"); err != nil {
		t.Fatal(err)
	}
	for _, agent := range plan.Agents {
		ticket, err := NewExecutionTicket(ExecutionTicketRequest{
			Handle: agent.Handle, LaunchNonce: nonce, Mode: agent.AdapterMode, Provider: agent.Handle,
			ProjectRoot: mainCwd, SessionRoot: sessionRoot, Cwd: agent.Cwd,
			ProviderExecutable: provider, AMQExecutable: amqBinary,
			TargetArgv: agent.Argv, TargetEnv: agent.EnvOverlay, Execution: agent.Execution,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteExecutionTicket(root, lease, ticket); err != nil {
			t.Fatal(err)
		}
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	result, err := Commands{}.Create(CreateRequest{ProjectRoot: mainCwd, Session: "collab", Plan: plan, AMQPath: amqBinary, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for i, command := range result.Commands {
		rootAt := indexOf(command.Argv, "--root")
		if rootAt < 0 || command.Argv[rootAt+1] != root.Base() {
			t.Fatalf("command[%d] root = %#v, want %q", i, command.Argv, root.Base())
		}
		if slices.Contains(command.Argv, "--session") {
			t.Fatalf("command[%d] emitted --session: %#v", i, command.Argv)
		}
	}
	for i, command := range result.Commands {
		cmd := exec.Command(command.Argv[0], command.Argv[1:]...)
		cmd.Dir = command.Cwd
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + os.TempDir(), "AMQ_NO_UPDATE_CHECK=1"}
		for key, value := range command.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run emitted command[%d]: %v\n%s", i, err, output)
		}
		if i == 1 && !strings.Contains(string(output), "exact-root-message") {
			t.Fatalf("sibling command did not drain the real message:\n%s", output)
		}
	}
	if _, err := os.Stat(filepath.Join(siblingCwd, ".agent-mail")); !os.IsNotExist(err) {
		t.Fatalf("sibling-local queue exists under %s: %v", siblingCwd, err)
	}
}

func runCommandsTestProcess(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}

func TestCommandsConformance(t *testing.T) {
	RunConformance(t, Commands{})
}

func assertCoopExecGrammar(t *testing.T, argv []string, sessionRoot string, handle string, planArgv []string) {
	t.Helper()
	if err := coopExecGrammarError(argv, sessionRoot, handle, planArgv); err != nil {
		t.Fatal(err)
	}
}

func coopExecGrammarError(argv []string, sessionRoot string, handle string, planArgv []string) error {
	root := indexOf(argv, "--root")
	if root < 0 || root+1 >= len(argv) {
		return fmt.Errorf("missing --root in %#v", argv)
	}
	if argv[root+1] != sessionRoot {
		return fmt.Errorf("--root value = %q, want %q", argv[root+1], sessionRoot)
	}
	if indexOf(argv, "--session") >= 0 {
		return fmt.Errorf("exact-root command also emitted --session: %#v", argv)
	}
	me := indexOf(argv, "--me")
	if me < 0 || me+1 >= len(argv) {
		return fmt.Errorf("missing --me in %#v", argv)
	}
	if argv[me+1] != handle {
		return fmt.Errorf("--me value = %q, want %q", argv[me+1], handle)
	}
	if me+2 >= len(argv) {
		return fmt.Errorf("missing command positional after --me")
	}
	command := indexOf(argv[me+2:], planArgv[0])
	if command < 0 {
		return fmt.Errorf("missing command positional %q after --me", planArgv[0])
	}
	command += me + 2
	if argv[me+2] == "--" {
		return fmt.Errorf("old shape: -- immediately after --me (command positional missing): %#v", argv)
	}
	dash := indexOf(argv, "--")
	if len(planArgv) == 1 {
		if dash >= 0 {
			return fmt.Errorf("lone command must omit --: %#v", argv)
		}
		return nil
	}
	if dash != command+1 {
		return fmt.Errorf("-- at %d, want immediately after command positional in %#v", dash, argv)
	}
	if !slices.Equal(argv[dash+1:], planArgv[1:]) {
		return fmt.Errorf("argv tail = %#v, want %#v", argv[dash+1:], planArgv[1:])
	}
	return nil
}

func indexOf(argv []string, want string) int {
	for i, v := range argv {
		if v == want {
			return i
		}
	}
	return -1
}

func countArg(argv []string, want string) int {
	count := 0
	for _, arg := range argv {
		if arg == want {
			count++
		}
	}
	return count
}
