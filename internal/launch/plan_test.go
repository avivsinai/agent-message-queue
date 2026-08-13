package launch

import (
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
