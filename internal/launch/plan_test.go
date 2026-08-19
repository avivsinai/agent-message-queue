package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func validPlan() Plan {
	return Plan{Version: PlanVersion, Agents: []AgentPlan{
		{
			Handle: "claude", Argv: []string{"/usr/local/bin/claude", "--model", "opus", "--session-id", "claude:one"},
			EnvOverlay: map[string]string{"LANG": "C", "TERM": "xterm"}, Cwd: "/work/project",
			AdapterMode: AdapterModeMint, ResumePolicy: ResumeEnabled,
			LaunchNonce: "launch-one", ConversationID: "claude:one",
			DynamicArgv: []DynamicArg{{Index: 4, Kind: DynamicArgConversationID}},
		},
		{
			Handle: "codex", Argv: []string{"/usr/local/bin/codex", "--launch-nonce", "launch-one"}, Cwd: "/work/project",
			AdapterMode: AdapterModeCapture, ResumePolicy: ResumeFresh,
			LaunchNonce: "launch-one", ConversationID: "codex:one",
			DynamicArgv: []DynamicArg{{Index: 2, Kind: DynamicArgLaunchNonce}},
		},
	}}
}

func TestInitialInputChangesPlanButNotTrustProjection(t *testing.T) {
	makePlan := func(text string) Plan {
		plan := validPlan()
		sum := sha256.Sum256([]byte(text))
		plan.Agents[0].Argv = append(plan.Agents[0].Argv, text)
		plan.Agents[0].InitialInput = &PlannedInitialInput{
			Kind: InitialInputArgument, SHA256: "sha256:" + hex.EncodeToString(sum[:]), ArgvIndex: len(plan.Agents[0].Argv) - 1,
		}
		return plan
	}
	first, second := makePlan("bootstrap one"), makePlan("bootstrap two")
	firstPlan, err := first.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := second.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if firstPlan == secondPlan {
		t.Fatal("initial input content did not change plan digest")
	}
	firstTrust, err := first.TrustSemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	secondTrust, err := second.TrustSemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if firstTrust != secondTrust {
		t.Fatalf("initial input content changed trust digest: %s != %s", firstTrust, secondTrust)
	}
	second.Agents[0].InitialInput.Kind = InitialInputFile
	if _, err := second.TrustSemanticDigest(); err == nil || !strings.Contains(err.Error(), "invalid initial input kind") {
		t.Fatalf("unsupported carrier kind was accepted: %v", err)
	}
}

func TestExecutionOptionsChangePlanAndTrustDigests(t *testing.T) {
	base := validPlan()
	base.Agents[0].Execution = &PrepareExecutionOptions{
		RequireWake: true, NoGitignore: true, WakeMode: "enabled", AuditReason: "baseline",
		InjectorMode: "raw", InjectorVia: "/opt/injector", InjectorArgs: []string{"send"},
		SymphonyEvents: []string{"after_create"}, SymphonyWorkspaceKey: "workspace-a",
	}
	planDigest, err := base.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	trustDigest, err := base.TrustSemanticDigest()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*PrepareExecutionOptions)
	}{
		{"require wake", func(options *PrepareExecutionOptions) { options.RequireWake = false }},
		{"no gitignore", func(options *PrepareExecutionOptions) { options.NoGitignore = false }},
		{"wake mode", func(options *PrepareExecutionOptions) { options.WakeMode = "disabled" }},
		{"audit reason", func(options *PrepareExecutionOptions) { options.AuditReason = "changed" }},
		{"injector mode", func(options *PrepareExecutionOptions) { options.InjectorMode = "paste" }},
		{"injector via", func(options *PrepareExecutionOptions) { options.InjectorVia = "/opt/other" }},
		{"injector args", func(options *PrepareExecutionOptions) { options.InjectorArgs[0] = "paste" }},
		{"symphony events", func(options *PrepareExecutionOptions) { options.SymphonyEvents[0] = "before_run" }},
		{"symphony workspace", func(options *PrepareExecutionOptions) { options.SymphonyWorkspaceKey = "workspace-b" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.Agents = append([]AgentPlan(nil), base.Agents...)
			candidate.Agents[0].Execution = clonePrepareExecutionOptions(base.Agents[0].Execution)
			test.mutate(candidate.Agents[0].Execution)
			gotPlan, err := candidate.SemanticDigest()
			if err != nil {
				t.Fatal(err)
			}
			gotTrust, err := candidate.TrustSemanticDigest()
			if err != nil {
				t.Fatal(err)
			}
			if gotPlan == planDigest || gotTrust == trustDigest {
				t.Fatalf("execution change did not invalidate both digests: plan=%t trust=%t", gotPlan != planDigest, gotTrust != trustDigest)
			}
		})
	}

	identical := base
	identical.Agents = append([]AgentPlan(nil), base.Agents...)
	identical.Agents[0].Execution = clonePrepareExecutionOptions(base.Agents[0].Execution)
	identicalPlan, err := identical.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	identicalTrust, err := identical.TrustSemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if identicalPlan != planDigest || identicalTrust != trustDigest {
		t.Fatalf("identical execution options changed digests: plan=%s/%s trust=%s/%s", identicalPlan, planDigest, identicalTrust, trustDigest)
	}
}

func TestPlanRejectsUnsafeInitialInputAndEnvironmentKey(t *testing.T) {
	base := validPlan()
	text := "safe"
	sum := sha256.Sum256([]byte(text))
	base.Agents[0].Argv = append(base.Agents[0].Argv, text)
	base.Agents[0].InitialInput = &PlannedInitialInput{Kind: InitialInputArgument, SHA256: "sha256:" + hex.EncodeToString(sum[:]), ArgvIndex: len(base.Agents[0].Argv) - 1}
	for _, value := range []string{"-option", "line\nfeed", "tab\tvalue", "delete\x7f"} {
		t.Run(fmt.Sprintf("initial input %q", value), func(t *testing.T) {
			candidate := base
			candidate.Agents = append([]AgentPlan(nil), base.Agents...)
			candidate.Agents[0].Argv = append([]string(nil), base.Agents[0].Argv...)
			candidate.Agents[0].InitialInput = &PlannedInitialInput{Kind: InitialInputArgument, ArgvIndex: len(candidate.Agents[0].Argv) - 1}
			candidate.Agents[0].Argv[candidate.Agents[0].InitialInput.ArgvIndex] = value
			valueSum := sha256.Sum256([]byte(value))
			candidate.Agents[0].InitialInput.SHA256 = "sha256:" + hex.EncodeToString(valueSum[:])
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), "initial input") {
				t.Fatalf("unsafe initial input validation error = %v", err)
			}
		})
	}

	for _, key := range []string{"FOO;BAR", "1FOO", "FOO-BAR"} {
		t.Run("environment key "+key, func(t *testing.T) {
			candidate := validPlan()
			candidate.Agents[0].EnvOverlay = map[string]string{key: "value"}
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), "invalid environment key") {
				t.Fatalf("unsafe environment key validation error = %v", err)
			}
		})
	}
}

func TestDefaultExecutionOptionsPreserveLegacyDigest(t *testing.T) {
	const want = "sha256:ce711c6541227a6093d8bc6ac33aba32f49b3e683d61bfe781d7d55b0029d8e6"
	for _, options := range []*PrepareExecutionOptions{
		nil,
		{},
		{WakeMode: "enabled"},
		{InjectorMode: "none"},
		{WakeMode: "enabled", InjectorMode: "none"},
	} {
		plan := validPlan()
		plan.Agents[0].Execution = options
		got, err := plan.SemanticDigest()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("default execution changed legacy digest: got %s want %s", got, want)
		}
		trust, err := plan.TrustSemanticDigest()
		if err != nil {
			t.Fatal(err)
		}
		if trust != want {
			t.Fatalf("default execution changed legacy trust digest: got %s want %s", trust, want)
		}
	}

	tests := []struct {
		name   string
		mutate func(*PrepareExecutionOptions)
	}{
		{"require wake", func(options *PrepareExecutionOptions) { options.RequireWake = true }},
		{"no gitignore", func(options *PrepareExecutionOptions) { options.NoGitignore = true }},
		{"wake mode", func(options *PrepareExecutionOptions) { options.WakeMode = "disabled" }},
		{"audit reason", func(options *PrepareExecutionOptions) { options.AuditReason = "audit" }},
		{"injector mode", func(options *PrepareExecutionOptions) { options.InjectorMode = "raw" }},
		{"injector via", func(options *PrepareExecutionOptions) { options.InjectorVia = "/opt/injector" }},
		{"injector args", func(options *PrepareExecutionOptions) { options.InjectorArgs = []string{"send"} }},
		{"symphony events", func(options *PrepareExecutionOptions) { options.SymphonyEvents = []string{"after_create"} }},
		{"symphony workspace", func(options *PrepareExecutionOptions) { options.SymphonyWorkspaceKey = "workspace" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := validPlan()
			plan.Agents[0].Execution = &PrepareExecutionOptions{}
			test.mutate(plan.Agents[0].Execution)
			got, err := plan.SemanticDigest()
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("non-default execution retained legacy digest %s", got)
			}
		})
	}
}

func TestSemanticDigestCanonicalAndExcludesRuntimeValues(t *testing.T) {
	first := validPlan()
	second := validPlan()
	second.Agents[0], second.Agents[1] = second.Agents[1], second.Agents[0]
	second.Agents[0].LaunchNonce = "launch-two"
	second.Agents[0].Argv[2] = "launch-two"
	second.Agents[0].ConversationID = "codex:two"
	second.Agents[1].LaunchNonce = "launch-two"
	second.Agents[1].ConversationID = "claude:two"
	second.Agents[1].Argv[4] = "claude:two"
	second.Agents[1].EnvOverlay = map[string]string{"TERM": "xterm", "LANG": "C"}

	want, err := first.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cosmetic/runtime changes changed digest: %s != %s", got, want)
	}

	second.Agents[1].Argv = append(second.Agents[1].Argv, "--dangerously-skip-permissions")
	changed, err := second.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if changed == want {
		t.Fatal("semantic argv change did not invalidate digest")
	}
}

func TestDynamicArgValidationPreventsDigestExclusionsFromLying(t *testing.T) {
	plan := validPlan()
	tests := []struct {
		name   string
		mutate func(*AgentPlan)
		want   string
	}{
		{"out of range", func(agent *AgentPlan) { agent.DynamicArgv[0].Index = 99 }, "outside argv"},
		{"executable", func(agent *AgentPlan) { agent.DynamicArgv[0].Index = 0 }, "argv[0] executable must be static"},
		{"duplicate index", func(agent *AgentPlan) { agent.DynamicArgv = append(agent.DynamicArgv, agent.DynamicArgv[0]) }, "duplicate index"},
		{"unknown kind", func(agent *AgentPlan) { agent.DynamicArgv[0].Kind = "other" }, "invalid kind"},
		{"mismatched value", func(agent *AgentPlan) { agent.Argv[4] = "another-id" }, "does not match"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyPlan := plan
			copyPlan.Agents = append([]AgentPlan(nil), plan.Agents...)
			copyPlan.Agents[0].Argv = append([]string(nil), plan.Agents[0].Argv...)
			copyPlan.Agents[0].DynamicArgv = append([]DynamicArg(nil), plan.Agents[0].DynamicArgv...)
			test.mutate(&copyPlan.Agents[0])
			if _, err := copyPlan.SemanticDigest(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("SemanticDigest error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDecodePlanStrictAndVersioned(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"malformed", `{`, "decode launch plan"},
		{"unknown field", `{"version":1,"agents":[],"extra":true}`, "unknown field"},
		{"downgrade", `{"version":0,"agents":[]}`, "unsupported launch plan version 0"},
		{"future", `{"version":2,"agents":[]}`, "unsupported launch plan version 2"},
		{"duplicate handle", `{"version":1,"agents":[{"handle":"a","argv":["a"],"cwd":"/x","adapter_mode":"mint","resume_policy":"resume"},{"handle":"a","argv":["a"],"cwd":"/x","adapter_mode":"capture","resume_policy":"fresh"}]}`, "duplicate agent handle"},
		{"invalid handle", `{"version":1,"agents":[{"handle":"../a","argv":["a"],"cwd":"/x","adapter_mode":"mint","resume_policy":"resume"}]}`, "invalid handle"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodePlan([]byte(test.json))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodePlan error = %v, want %q", err, test.want)
			}
		})
	}
}
