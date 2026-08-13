package launch

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
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
		assertCoopExecGrammar(t, cmd.Argv, agent.Handle, agent.Argv)
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
	if err := WriteBinding(root, record); err != nil {
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
	result, err := Commands{}.Create(CreateRequest{Session: "collab", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(result.Commands))
	}
	assertCoopExecGrammar(t, result.Commands[0].Argv, "claude", plan.Agents[0].Argv)
	assertCoopExecGrammar(t, result.Commands[1].Argv, "codex", plan.Agents[1].Argv)
	if slices.Contains(result.Commands[1].Argv, "--") {
		t.Fatalf("lone executable must omit --: %#v", result.Commands[1].Argv)
	}
	if !strings.Contains(result.Commands[0].Line, "/usr/local/bin/claude --") {
		t.Fatalf("Line missing command positional before --: %s", result.Commands[0].Line)
	}
}

func TestCommandsCoopExecOldShapeFailsGrammar(t *testing.T) {
	old := []string{"amq", "coop", "exec", "--session", "collab", "--me", "claude", "--", "/usr/local/bin/claude", "--model", "opus"}
	if err := coopExecGrammarError(old, "claude", []string{"/usr/local/bin/claude", "--model", "opus"}); err == nil {
		t.Fatal("old shape (-- immediately after --me) passed grammar check")
	}
}

func TestCommandsConformance(t *testing.T) {
	RunConformance(t, Commands{})
}

func assertCoopExecGrammar(t *testing.T, argv []string, handle string, planArgv []string) {
	t.Helper()
	if err := coopExecGrammarError(argv, handle, planArgv); err != nil {
		t.Fatal(err)
	}
}

func coopExecGrammarError(argv []string, handle string, planArgv []string) error {
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
	if argv[me+2] == "--" {
		return fmt.Errorf("old shape: -- immediately after --me (command positional missing): %#v", argv)
	}
	if argv[me+2] != planArgv[0] {
		return fmt.Errorf("command positional = %q, want %q", argv[me+2], planArgv[0])
	}
	dash := indexOf(argv, "--")
	if len(planArgv) == 1 {
		if dash >= 0 {
			return fmt.Errorf("lone command must omit --: %#v", argv)
		}
		return nil
	}
	if dash != me+3 {
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
