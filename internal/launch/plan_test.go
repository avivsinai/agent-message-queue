package launch

import (
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
