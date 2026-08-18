package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/launchapi"
)

func TestPublicLaunchUnsupportedIsExit6AndZeroWrite(t *testing.T) {
	for _, args := range [][]string{
		{"--launcher", "cmux", "--json"},
		{"--launcher", "cmux", "--prepare", "--json"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			project, _ := launchCLIFixture(t, "collab")
			intent := launchapi.LaunchIntentV1{
				IntentVersion: launchapi.IntentVersionV1,
				Participants:  []launchapi.ParticipantV1{{Handle: "claude", Runnable: false}},
			}
			data, err := launchapi.MarshalLaunchIntentV1(intent)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "intent.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			before := strings.Join(snapshotTree(t, project), "\n")
			commandArgs := append([]string{"--plan", path}, args...)
			stdout, _, cliErr := captureEnvOutput(t, func() error { return runLaunch(commandArgs) })
			if GetExitCode(cliErr) != ExitActionRequired {
				t.Fatalf("unsupported exit=%d err=%v\n%s", GetExitCode(cliErr), cliErr, stdout)
			}
			var result launchapi.PrepareResultV1
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			if result.Outcome != launchapi.PrepareOutcomeUnsupported || result.Reason != "launcher_not_available" {
				t.Fatalf("unsupported result = %#v", result)
			}
			if after := strings.Join(snapshotTree(t, project), "\n"); after != before {
				t.Fatalf("unsupported Prepare changed the project tree\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestPublicLaunchApplyReadsFullRequest(t *testing.T) {
	project, _ := launchCLIFixture(t, "collab")
	request := launchapi.PrepareRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Target: launchapi.TargetV1{
			ProjectRoot: project,
			SessionRoot: filepath.Join(project, defaultCoopRoot, "collab"),
			Session:     "collab",
		},
		Launcher: "commands",
		Intent: launchapi.LaunchIntentV1{IntentVersion: launchapi.IntentVersionV1, Participants: []launchapi.ParticipantV1{{
			Handle: "operator", Runnable: false,
		}}},
	}
	prepared, err := launchapi.Prepare(context.Background(), request)
	if err != nil || prepared.Outcome != launchapi.PrepareOutcomeReady || len(prepared.RequiredActions) != 0 {
		t.Fatalf("Prepare = %#v, %v", prepared, err)
	}
	apply := launchapi.ApplyRequestV1{
		RequestVersion: launchapi.RequestVersionV1, Prepare: request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: []launchapi.DecisionV1{},
	}
	data, err := json.Marshal(apply)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "apply.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, cliErr := captureEnvOutput(t, func() error { return runLaunch([]string{"--apply", path, "--json"}) })
	if cliErr != nil {
		t.Fatalf("CLI Apply: %v\n%s", cliErr, stdout)
	}
	var result launchapi.ApplyResultV1
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "provisioned_no_runnable" || result.SemanticDigest != result.TrustDigest || len(result.Roster.Desired) != 1 || result.Roster.Desired[0] != "operator" {
		t.Fatalf("CLI Apply result = %#v", result)
	}
}

func TestPublicLaunchPlanNeverAutoAuthorizesRequiredActions(t *testing.T) {
	project, _ := launchCLIFixture(t, "collab")
	provider := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\ncase \"$1\" in --version) echo '1.0.0 (Claude Code)' ;; --help) echo '--session-id --resume' ;; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	intent := launchapi.LaunchIntentV1{
		IntentVersion: launchapi.IntentVersionV1,
		Participants: []launchapi.ParticipantV1{{
			Handle: "claude", Runnable: true, Executable: provider,
			Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: project},
			ResumePolicy: launchapi.ResumePolicyFresh,
			Execution: &launchapi.ExecutionOptionsV1{
				Wake: launchapi.WakeOptionsV1{Mode: launchapi.WakeDisabled, AuditReason: "test fixture"},
			},
		}},
	}
	data, err := launchapi.MarshalLaunchIntentV1(intent)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "intent.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sessionRoot := filepath.Join(project, defaultCoopRoot, "collab")
	before := strings.Join(snapshotTree(t, project), "\n")
	stdout, _, cliErr := captureEnvOutput(t, func() error { return runLaunch([]string{"--plan", path, "--json"}) })
	if GetExitCode(cliErr) != ExitActionRequired {
		t.Fatalf("plain --plan exit=%d err=%v\n%s", GetExitCode(cliErr), cliErr, stdout)
	}
	var result launchapi.PrepareResultV1
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != launchapi.PrepareOutcomeActionRequired || len(result.RequiredActions) == 0 || result.RequiredActions[0].Kind != launchapi.RequiredActionTrustConfirmation {
		t.Fatalf("plain --plan result = %#v", result)
	}
	if after := strings.Join(snapshotTree(t, project), "\n"); after != before {
		t.Fatalf("action-required --plan changed the project tree\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(sessionRoot, "meta", "launch", "binding.json")); !os.IsNotExist(err) {
		t.Fatalf("action-required --plan wrote a binding: %v", err)
	}
}

func TestPublicLaunchPlanAppliesWithEmptyDecisionsWhenReady(t *testing.T) {
	_, _ = launchCLIFixture(t, "collab")
	intent := launchapi.LaunchIntentV1{
		IntentVersion: launchapi.IntentVersionV1,
		Participants:  []launchapi.ParticipantV1{{Handle: "operator", Runnable: false}},
	}
	data, err := launchapi.MarshalLaunchIntentV1(intent)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "intent.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, cliErr := captureEnvOutput(t, func() error { return runLaunch([]string{"--plan", path, "--json"}) })
	if cliErr != nil {
		t.Fatalf("plain --plan: %v\n%s", cliErr, stdout)
	}
	var result launchapi.ApplyResultV1
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "provisioned_no_runnable" || result.SemanticDigest != result.TrustDigest || len(result.Roster.Desired) != 1 || result.Roster.Desired[0] != "operator" {
		t.Fatalf("plain --plan result = %#v", result)
	}
}

func TestPublicLaunchModeFlagRefusals(t *testing.T) {
	launchCLIFixture(t, "collab")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "prepare without plan", args: []string{"--prepare", "--json"}, want: "--prepare requires --plan"},
		{name: "prepare without json", args: []string{"--plan", "intent.json", "--prepare"}, want: "--prepare requires --json"},
		{name: "apply without json", args: []string{"--apply", "apply.json"}, want: "--apply requires --json"},
		{name: "mixed apply and plan", args: []string{"--apply", "apply.json", "--plan", "intent.json", "--json"}, want: "mutually exclusive"},
		{name: "apply target override", args: []string{"--apply", "apply.json", "--session", "collab", "--json"}, want: "takes its target"},
		{name: "legacy decision flag", args: []string{"--plan", "intent.json", "--fresh"}, want: "legacy launch decision flags"},
		{name: "require agent is legacy only", args: []string{"--plan", "intent.json", "--require-agent"}, want: "legacy launch decision flags"},
		{name: "rebind is legacy only", args: []string{"--plan", "intent.json", "--rebind"}, want: "legacy launch decision flags"},
		{name: "allow fresh is legacy only", args: []string{"--plan", "intent.json", "--allow-fresh-fallback"}, want: "legacy launch decision flags"},
		{name: "mixed apply and prepare", args: []string{"--apply", "apply.json", "--prepare", "--plan", "intent.json", "--json"}, want: "mutually exclusive"},
		{name: "apply and prepare without plan", args: []string{"--apply", "apply.json", "--prepare", "--json"}, want: "mutually exclusive"},
		{name: "apply launcher override", args: []string{"--apply", "apply.json", "--launcher", "commands", "--json"}, want: "takes its target"},
		{name: "placement without plan", args: []string{"--placement", `{"target":"session","layout":"columns"}`, "--json"}, want: "--placement requires --plan"},
		{name: "apply with placement flag", args: []string{"--apply", "apply.json", "--json", "--placement", `{"target":"session","layout":"columns"}`}, want: "takes placement"},
		{name: "empty placement", args: []string{"--plan", "intent.json", "--prepare", "--json", "--placement", ""}, want: "--placement must be a PlacementV1 JSON object"},
		{name: "whitespace placement", args: []string{"--plan", "intent.json", "--prepare", "--json", "--placement", " \t"}, want: "--placement must be a PlacementV1 JSON object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := runLaunch(test.args)
			if GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want usage containing %q", err, test.want)
			}
		})
	}
}

func TestPublicLaunchPlacementRejectsTrailingJSON(t *testing.T) {
	launchCLIFixture(t, "collab")
	intent := launchapi.LaunchIntentV1{
		IntentVersion: launchapi.IntentVersionV1,
		Participants:  []launchapi.ParticipantV1{{Handle: "operator", Runnable: false}},
	}
	data, err := launchapi.MarshalLaunchIntentV1(intent)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "intent.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = runLaunch([]string{
		"--plan", path, "--prepare", "--json", "--launcher", "commands",
		"--placement", `{"target":"session","layout":"columns"} {"target":"new_window","layout":"rows"}`,
	})
	if GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("trailing placement JSON error = %v", err)
	}
}

func TestCLIResultEncoderUsesPackageGoldens(t *testing.T) {
	for _, test := range []struct {
		file   string
		result any
	}{
		{file: "prepare_result_v1.golden.json", result: &launchapi.PrepareResultV1{}},
		{file: "apply_result_v1.golden.json", result: &launchapi.ApplyResultV1{}},
	} {
		golden, err := os.ReadFile(filepath.Join("..", "..", "launchapi", "testdata", test.file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(golden, test.result); err != nil {
			t.Fatal(err)
		}
		stdout, _, err := captureEnvOutput(t, func() error { return outputPublicLaunchResult(dereferenceGoldenResult(test.result)) })
		if err != nil {
			t.Fatal(err)
		}
		if stdout != string(golden) {
			t.Fatalf("CLI output differs from shared %s\ngot:\n%s\nwant:\n%s", test.file, stdout, golden)
		}
	}
}

func TestPublicApplyFailureDetailIsStderrOnly(t *testing.T) {
	result := launchapi.ApplyResultV1{
		ResultVersion: launchapi.ResultVersionV1,
		Outcome:       "action_required",
		ReasonCode:    "backend_create_failed",
		FailureDetail: "create tmux session: pane exited before inspection",
	}
	encoded, err := launchapi.MarshalResultV1(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "pane exited") || strings.Contains(string(encoded), "failure_detail") {
		t.Fatalf("failure detail leaked into public JSON: %s", encoded)
	}
	_, stderr, err := captureEnvOutput(t, func() error { return publicApplyExit(result) })
	if GetExitCode(err) != ExitActionRequired || !strings.Contains(stderr, result.FailureDetail) {
		t.Fatalf("exit=%d err=%v stderr=%q", GetExitCode(err), err, stderr)
	}
}

func dereferenceGoldenResult(result any) any {
	switch value := result.(type) {
	case *launchapi.PrepareResultV1:
		return *value
	case *launchapi.ApplyResultV1:
		return *value
	default:
		return result
	}
}

func TestCLIAndPackageResultGoldenParity(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	project, _ := launchCLIFixture(t, "collab")
	intent := launchapi.LaunchIntentV1{
		IntentVersion: launchapi.IntentVersionV1,
		Participants:  []launchapi.ParticipantV1{{Handle: "claude", Runnable: false}},
	}
	intentData, err := launchapi.MarshalLaunchIntentV1(intent)
	if err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(t.TempDir(), "intent.json")
	if err := os.WriteFile(intentPath, intentData, 0o600); err != nil {
		t.Fatal(err)
	}
	before := strings.Join(snapshotTree(t, project), "\n")
	stdout, _, cliErr := captureEnvOutput(t, func() error {
		return runLaunch([]string{"--plan", intentPath, "--prepare", "--json"})
	})
	if cliErr != nil {
		t.Fatalf("CLI Prepare: %v\n%s", cliErr, stdout)
	}
	if after := strings.Join(snapshotTree(t, project), "\n"); after != before {
		t.Fatalf("CLI Prepare changed the project tree\nbefore:\n%s\nafter:\n%s", before, after)
	}
	request := launchapi.PrepareRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Target: launchapi.TargetV1{
			ProjectRoot: project,
			SessionRoot: filepath.Join(project, defaultCoopRoot, "collab"),
			Session:     "collab",
		},
		Launcher: "auto",
		Intent:   intent,
	}
	packageResult, err := launchapi.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	packageJSON, err := launchapi.MarshalResultV1(packageResult)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(packageJSON) {
		t.Fatalf("CLI/package JSON differ\nCLI:\n%s\npackage:\n%s", stdout, packageJSON)
	}
	t.Run("digests present", func(t *testing.T) {
		if packageResult.PlanDigest == "" || packageResult.TrustDigest == "" || packageResult.SubjectDigest == "" {
			t.Fatalf("result omitted required digests: %#v", packageResult)
		}
	})
	t.Run("root-only change", func(t *testing.T) {
		otherSession := filepath.Join(project, "other-mail", "collab")
		if err := fsq.EnsureRootDirs(otherSession); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(otherSession, "claude"); err != nil {
			t.Fatal(err)
		}
		changedRequest := request
		changedRequest.Target.SessionRoot = otherSession
		changed, err := launchapi.Prepare(context.Background(), changedRequest)
		if err != nil {
			t.Fatal(err)
		}
		if changed.PlanDigest != packageResult.PlanDigest || changed.TrustDigest == packageResult.TrustDigest || changed.SubjectDigest == packageResult.SubjectDigest {
			t.Fatalf("root-only digest change: plan=%t trust=%t subject=%t", changed.PlanDigest != packageResult.PlanDigest, changed.TrustDigest != packageResult.TrustDigest, changed.SubjectDigest != packageResult.SubjectDigest)
		}
	})
	t.Run("binding-only change", func(t *testing.T) {
		writeCommandsBinding(t, request.Target.SessionRoot)
		changed, err := launchapi.Prepare(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if changed.PlanDigest != packageResult.PlanDigest || changed.TrustDigest != packageResult.TrustDigest || changed.SubjectDigest == packageResult.SubjectDigest {
			t.Fatalf("binding-only digest change: plan=%t trust=%t subject=%t", changed.PlanDigest != packageResult.PlanDigest, changed.TrustDigest != packageResult.TrustDigest, changed.SubjectDigest != packageResult.SubjectDigest)
		}
	})
	t.Run("semantic digest alias", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(repoRoot, "launchapi", "testdata", "apply_result_v1.golden.json"))
		if err != nil {
			t.Fatal(err)
		}
		var result launchapi.ApplyResultV1
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.SemanticDigest == "" || result.SemanticDigest != result.TrustDigest {
			t.Fatalf("semantic_digest=%q trust_digest=%q", result.SemanticDigest, result.TrustDigest)
		}
	})
}
