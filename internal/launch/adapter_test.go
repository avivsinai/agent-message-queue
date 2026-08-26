package launch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	testLaunchNonce    = "018f1f29-7b58-7cc3-98f1-9a74b7d01733"
	testConversationID = "018f1f2a-bc34-71bd-9056-23838e27f859"
)

type fakeCommandProbe struct {
	path    string
	lookErr error
	outputs map[string]string
	errors  map[string]error
}

func (probe fakeCommandProbe) LookPath(string) (string, error) { return probe.path, probe.lookErr }
func (probe fakeCommandProbe) Output(_ context.Context, _ string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	return []byte(probe.outputs[key]), probe.errors[key]
}

func testExecutable(t *testing.T, name string) (string, string) {
	t.Helper()
	base := t.TempDir()
	project := filepath.Join(base, "project")
	bin := filepath.Join(base, "bin")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	target := "/usr/bin/true"
	if _, err := os.Stat(target); err != nil {
		target = "/bin/true"
	}
	executable := filepath.Join(bin, name)
	if err := os.Symlink(target, executable); err != nil {
		t.Fatal(err)
	}
	return project, executable
}

func planRequest(project, handle string) PlanRequest {
	return PlanRequest{
		Handle: handle, ProjectRoot: project, Cwd: project,
		SessionRoot: project, AMQExecutable: "/usr/bin/true",
		LaunchNonce: testLaunchNonce, ResumePolicy: ResumeEnabled,
		EnvOverlay: map[string]string{"LANG": "en_US.UTF-8", "TERM": "xterm-256color"},
	}
}

func TestAdapterCapabilitiesProbeExactIdentityContracts(t *testing.T) {
	claude := NewClaudeAdapter("claude")
	claude.probe = fakeCommandProbe{path: "/bin/claude", outputs: map[string]string{
		"--version": "2.1.229 (Claude Code)\n",
		"--help":    "--session-id <uuid> --resume [value]",
	}}
	gotClaude := claude.Capabilities(context.Background())
	if !gotClaude.Available || !gotClaude.Fresh || !gotClaude.Resume || gotClaude.Capture || gotClaude.Mode != AdapterModeMint {
		t.Fatalf("Claude capabilities = %#v", gotClaude)
	}
	if gotClaude.ProviderVersion != "2.1.229" {
		t.Fatalf("Claude provider version = %q", gotClaude.ProviderVersion)
	}
	if err := ValidateAdapterCapabilities(claude, gotClaude); err != nil {
		t.Fatal(err)
	}

	grok := NewGrokAdapter("grok")
	grok.probe = fakeCommandProbe{path: "/bin/grok", outputs: map[string]string{
		"--version": "Grok Build 1.0.5\n",
		"--help":    "-s, --session-id <UUID> -r, --resume [<ID>]",
	}}
	gotGrok := grok.Capabilities(context.Background())
	if !gotGrok.Available || !gotGrok.Fresh || !gotGrok.Resume || gotGrok.Capture || gotGrok.Mode != AdapterModeMint {
		t.Fatalf("Grok capabilities = %#v", gotGrok)
	}
	if gotGrok.ProviderVersion != grokBuildVersion {
		t.Fatalf("Grok provider version = %q", gotGrok.ProviderVersion)
	}
	if err := ValidateAdapterCapabilities(grok, gotGrok); err != nil {
		t.Fatal(err)
	}

	codex := NewCodexAdapter("codex")
	codex.probe = fakeCommandProbe{path: "/bin/codex", outputs: map[string]string{
		"--version":     "codex-cli 0.147.0\n",
		"--help":        "commands: resume",
		"resume --help": "Usage: codex resume [OPTIONS] [SESSION_ID]",
	}}
	gotCodex := codex.Capabilities(context.Background())
	if !gotCodex.Available || !gotCodex.Fresh || !gotCodex.Resume || !gotCodex.Capture || gotCodex.PreSpawnAcquire || gotCodex.Mode != AdapterModeCapture {
		t.Fatalf("Codex capabilities = %#v", gotCodex)
	}
	if gotCodex.ProviderVersion != "0.147.0" {
		t.Fatalf("Codex provider version = %q", gotCodex.ProviderVersion)
	}
	if err := ValidateAdapterCapabilities(codex, gotCodex); err != nil {
		t.Fatal(err)
	}

	codex.probe = fakeCommandProbe{path: "/bin/codex", outputs: map[string]string{
		"--version": "codex-cli 0.147.0\n", "--help": "commands: resume",
		"resume --help": "--last only",
	}}
	unsupported := codex.Capabilities(context.Background())
	if unsupported.Available || unsupported.Reason != "identity_contract_unsupported" {
		t.Fatalf("unsupported Codex capabilities = %#v", unsupported)
	}
	codex.probe = fakeCommandProbe{path: "/bin/codex", outputs: map[string]string{
		"--version": "codex-cli 0.148.0\n", "--help": "commands: resume",
		"resume --help": "Usage: codex resume [OPTIONS] [SESSION_ID]",
	}}
	untestedVersion := codex.Capabilities(context.Background())
	if !untestedVersion.Available || !untestedVersion.Fresh || !untestedVersion.Resume || untestedVersion.Capture || untestedVersion.PreSpawnAcquire || untestedVersion.Reason != "capture_version_unsupported" {
		t.Fatalf("untested Codex version capabilities = %#v", untestedVersion)
	}
	if err := ValidateAdapterCapabilities(codex, untestedVersion); err != nil {
		t.Fatal(err)
	}
}

func TestClaudePlansUseExactMintAndResumeShapes(t *testing.T) {
	project, executable := testExecutable(t, ClaudeProvider)
	adapter := NewClaudeAdapter(executable)
	request := planRequest(project, ClaudeProvider)
	request.CommittedArgs = []string{"--model", "opus", "--permission-mode", "plan"}
	request.BypassArgs = []string{"--dangerously-skip-permissions"}

	fresh, err := adapter.PlanFresh(request)
	if err != nil {
		t.Fatal(err)
	}
	wantFreshTail := []string{"--session-id", testLaunchNonce}
	if !reflect.DeepEqual(fresh.Argv[len(fresh.Argv)-2:], wantFreshTail) {
		t.Fatalf("fresh argv = %q", fresh.Argv)
	}
	if fresh.AdapterMode != AdapterModeMint || fresh.ConversationID != testLaunchNonce ||
		len(fresh.DynamicArgv) != 1 || fresh.DynamicArgv[0].Kind != DynamicArgLaunchNonce {
		t.Fatalf("fresh plan = %#v", fresh)
	}

	resume, err := adapter.PlanResume(ResumeRequest{
		PlanRequest:  request,
		Conversation: ConversationIdentity{Provider: ClaudeProvider, ID: testConversationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantResumeTail := []string{"--resume", testConversationID}
	if !reflect.DeepEqual(resume.Argv[len(resume.Argv)-2:], wantResumeTail) {
		t.Fatalf("resume argv = %q", resume.Argv)
	}
	if resume.DynamicArgv[0].Kind != DynamicArgConversationID {
		t.Fatalf("resume dynamic argv = %#v", resume.DynamicArgv)
	}

	resume.Argv[resume.DynamicArgv[0].Index] = testLaunchNonce
	if err := ValidateAdapterPlan(adapter, resume); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("slot laundering error = %v", err)
	}
	if result := adapter.CaptureIdentity(CaptureRequest{}); result.State != CaptureUnsupported || result.Degraded || result.CanPersist() {
		t.Fatalf("mint capture result = %#v", result)
	}
}

func TestClaudeNamedFreshPlanIsTicketBoundAndResumeStable(t *testing.T) {
	project, executable := testExecutable(t, ClaudeProvider)
	adapter := NewClaudeAdapter(executable)
	request := planRequest(project, ClaudeProvider)
	request.CommittedArgs = []string{"--model", "opus"}
	request.Session = "session1"

	unnamed, err := adapter.PlanFresh(request)
	if err != nil {
		t.Fatal(err)
	}
	wantUnnamed := []string{executable, "--model", "opus", "--session-id", testLaunchNonce}
	if !slices.Equal(unnamed.Argv, wantUnnamed) {
		t.Fatalf("unnamed fresh argv = %#v, want %#v", unnamed.Argv, wantUnnamed)
	}
	legacyRequest := request
	legacyRequest.Session = ""
	legacy, err := adapter.PlanFresh(legacyRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy, unnamed) {
		t.Fatalf("unnamed fresh plan changed when session was supplied: legacy=%#v unnamed=%#v", legacy, unnamed)
	}
	namedRequest := request
	namedRequest.Named = true
	named, err := adapter.PlanFresh(namedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(named.Argv[len(named.Argv)-4:], []string{"--name", "session1/claude", "--session-id", testLaunchNonce}) {
		t.Fatalf("named fresh argv = %#v", named.Argv)
	}
	if countClaudeNameFlags(named.Argv) != 1 {
		t.Fatalf("named fresh argv has duplicate name flags: %#v", named.Argv)
	}
	planDigest := func(plan AgentPlan) (string, string) {
		t.Helper()
		container := Plan{Version: PlanVersion, Agents: []AgentPlan{plan}}
		semantic, semanticErr := container.SemanticDigest()
		if semanticErr != nil {
			t.Fatal(semanticErr)
		}
		trust, trustErr := container.TrustSemanticDigest()
		if trustErr != nil {
			t.Fatal(trustErr)
		}
		return semantic, trust
	}
	unnamedDigest, unnamedTrust := planDigest(unnamed)
	namedDigest, namedTrust := planDigest(named)
	if unnamedDigest == namedDigest || unnamedTrust == namedTrust {
		t.Fatalf("name did not change plan and trust digests: %q/%q", namedDigest, namedTrust)
	}

	resume, err := adapter.PlanResume(ResumeRequest{
		PlanRequest:  namedRequest,
		Conversation: ConversationIdentity{Provider: ClaudeProvider, ID: testConversationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if countClaudeNameFlags(resume.Argv) != 0 {
		t.Fatalf("resume added a generated name: %#v", resume.Argv)
	}

	committed := namedRequest
	committed.CommittedArgs = []string{"-n", "custom"}
	committedPlan, err := adapter.PlanFresh(committed)
	if err != nil {
		t.Fatal(err)
	}
	if countClaudeNameFlags(committedPlan.Argv) != 1 || slices.Contains(committedPlan.Argv, "--name") {
		t.Fatalf("committed short name was duplicated or rewritten: %#v", committedPlan.Argv)
	}

	inline := namedRequest
	inline.CommittedArgs = []string{"--name=custom"}
	inlinePlan, err := adapter.PlanFresh(inline)
	if err != nil {
		t.Fatal(err)
	}
	if countClaudeNameFlags(inlinePlan.Argv) != 1 {
		t.Fatalf("committed inline name was duplicated: %#v", inlinePlan.Argv)
	}
	invalid := namedRequest
	invalid.CommittedArgs = []string{"--name", "bad/name/extra"}
	if _, err := adapter.PlanFresh(invalid); err == nil || !strings.Contains(err.Error(), "invalid value") {
		t.Fatalf("invalid committed name error = %v", err)
	}
}

func countClaudeNameFlags(argv []string) int {
	count := 0
	for _, arg := range argv {
		if arg == "-n" || arg == "--name" || strings.HasPrefix(arg, "--name=") {
			count++
		}
	}
	return count
}

func TestGrokPlansUseExactMintAndResumeShapes(t *testing.T) {
	project, executable := testExecutable(t, GrokProvider)
	adapter := NewGrokAdapter(executable)
	request := planRequest(project, GrokProvider)
	request.CommittedArgs = []string{
		"--model", "grok-4.5", "--effort", "high", "--no-auto-update",
		"--tools", "shell,custom-provider-tool", "--disallowed-tools", "legacy_tool",
	}

	fresh, err := adapter.PlanFresh(request)
	if err != nil {
		t.Fatal(err)
	}
	wantFreshTail := []string{"--session-id", testLaunchNonce}
	if !reflect.DeepEqual(fresh.Argv[len(fresh.Argv)-2:], wantFreshTail) {
		t.Fatalf("fresh argv = %q", fresh.Argv)
	}
	if fresh.AdapterMode != AdapterModeMint || fresh.ConversationID != testLaunchNonce ||
		len(fresh.DynamicArgv) != 1 || fresh.DynamicArgv[0].Kind != DynamicArgLaunchNonce {
		t.Fatalf("fresh plan = %#v", fresh)
	}

	resume, err := adapter.PlanResume(ResumeRequest{
		PlanRequest:  request,
		Conversation: ConversationIdentity{Provider: GrokProvider, ID: testConversationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantResumeTail := []string{"--resume", testConversationID}
	if !reflect.DeepEqual(resume.Argv[len(resume.Argv)-2:], wantResumeTail) {
		t.Fatalf("resume argv = %q", resume.Argv)
	}
	if resume.DynamicArgv[0].Kind != DynamicArgConversationID {
		t.Fatalf("resume dynamic argv = %#v", resume.DynamicArgv)
	}

	resume.Argv[resume.DynamicArgv[0].Index] = testLaunchNonce
	if err := ValidateAdapterPlan(adapter, resume); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("slot laundering error = %v", err)
	}
	if result := adapter.CaptureIdentity(CaptureRequest{}); result.State != CaptureUnsupported || result.Degraded || result.CanPersist() {
		t.Fatalf("mint capture result = %#v", result)
	}
}

func TestGrokCommittedConfigRejectsDuplicateToolFlags(t *testing.T) {
	project := t.TempDir()
	adapter := NewGrokAdapter("definitely-not-installed-grok")
	for _, args := range [][]string{
		{"--tools", "shell", "--tools", "computer"},
		{"--disallowed-tools", "legacy_tool", "--disallowed-tools", "shell"},
	} {
		err := ValidateCommittedConfig(adapter, CommittedConfigRequest{ProjectRoot: project, Args: args})
		if err == nil || !strings.Contains(err.Error(), "duplicated") {
			t.Fatalf("ValidateCommittedConfig(%q) error = %v, want duplicated", args, err)
		}
	}
}

// TestGrokCommittedConfigRejectsBypassFlagAsToolValue confirms the leading-dash
// guard in validGrokToolList refuses a flag-looking value smuggled into
// --tools or --disallowed-tools (e.g. the Grok bypass flag), mirroring the
// Claude strictness-inversion fix in issue #648 item 2.
func TestGrokCommittedConfigRejectsBypassFlagAsToolValue(t *testing.T) {
	project := t.TempDir()
	adapter := NewGrokAdapter("definitely-not-installed-grok")
	for _, args := range [][]string{
		{"--tools", "--dangerously-bypass-approvals-and-sandbox"},
		{"--disallowed-tools", "--dangerously-bypass-approvals-and-sandbox"},
		{"--tools", "--verbose"},
	} {
		err := ValidateCommittedConfig(adapter, CommittedConfigRequest{ProjectRoot: project, Args: args})
		if err == nil || !strings.Contains(err.Error(), "invalid value") {
			t.Fatalf("ValidateCommittedConfig(%q) error = %v, want invalid value", args, err)
		}
	}
}

func TestGrokPlansForwardOpaqueToolNames(t *testing.T) {
	project, executable := testExecutable(t, GrokProvider)
	adapter := NewGrokAdapter(executable)
	request := planRequest(project, GrokProvider)
	request.CommittedArgs = []string{"--tools", "run_terminal_cmd(Agent);custom"}
	fresh, err := adapter.PlanFresh(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Argv[1:3], []string{"--tools", "run_terminal_cmd(Agent);custom"}) {
		t.Fatalf("opaque tool argv = %q", fresh.Argv)
	}
}

func TestGrokPlansRejectContinueIdentityAndBypassFlags(t *testing.T) {
	project, executable := testExecutable(t, GrokProvider)
	adapter := NewGrokAdapter(executable)
	for _, args := range [][]string{
		{"--continue"},
		{"--always-approve"},
		{"--yolo"},
		{"--allowedTools", "Bash,Read,Write"},
		{"--session-id", testConversationID},
		{"--resume", testConversationID},
	} {
		request := planRequest(project, GrokProvider)
		request.CommittedArgs = args
		if _, err := adapter.PlanFresh(request); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("PlanFresh(%q) error = %v, want deny-by-default refusal", args, err)
		}
	}
}

func TestAdaptersAppendTypedInitialInputAfterOwnedArguments(t *testing.T) {
	for _, provider := range []string{ClaudeProvider, CodexProvider, GrokProvider} {
		t.Run(provider, func(t *testing.T) {
			project, executable := testExecutable(t, provider)
			request := planRequest(project, provider)
			text := "generated bootstrap"
			sum := sha256.Sum256([]byte(text))
			request.InitialInput = &InitialInputRequest{Kind: InitialInputArgument, Value: text, SHA256: "sha256:" + hex.EncodeToString(sum[:])}
			var plan AgentPlan
			var err error
			switch provider {
			case ClaudeProvider:
				plan, err = NewClaudeAdapter(executable).PlanFresh(request)
			case CodexProvider:
				plan, err = NewCodexAdapter(executable).PlanFresh(request)
			case GrokProvider:
				plan, err = NewGrokAdapter(executable).PlanFresh(request)
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.Argv[len(plan.Argv)-1] != text || plan.InitialInput == nil || plan.InitialInput.ArgvIndex != len(plan.Argv)-1 {
				t.Fatalf("initial input plan = %#v", plan)
			}
		})
	}
}

func TestAdaptersRejectUnsafeInitialInputBeforePlanning(t *testing.T) {
	for _, provider := range []string{ClaudeProvider, CodexProvider, GrokProvider} {
		t.Run(provider, func(t *testing.T) {
			project, executable := testExecutable(t, provider)
			for _, text := range []string{"-option", "line\nfeed", "tab\tvalue", "delete\x7f"} {
				t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
					request := planRequest(project, provider)
					sum := sha256.Sum256([]byte(text))
					request.InitialInput = &InitialInputRequest{
						Kind: InitialInputArgument, Value: text, SHA256: "sha256:" + hex.EncodeToString(sum[:]),
					}
					var err error
					switch provider {
					case ClaudeProvider:
						_, err = NewClaudeAdapter(executable).PlanFresh(request)
					case CodexProvider:
						_, err = NewCodexAdapter(executable).PlanFresh(request)
					case GrokProvider:
						_, err = NewGrokAdapter(executable).PlanFresh(request)
					}
					if err == nil || !strings.Contains(err.Error(), "initial input") {
						t.Fatalf("unsafe initial input error = %v", err)
					}
				})
			}
		})
	}
}

func TestAdaptersDoNotRedeliverInitialInputOnResume(t *testing.T) {
	for _, provider := range []string{ClaudeProvider, CodexProvider, GrokProvider} {
		t.Run(provider, func(t *testing.T) {
			project, executable := testExecutable(t, provider)
			request := planRequest(project, provider)
			text := "generated bootstrap"
			sum := sha256.Sum256([]byte(text))
			request.InitialInput = &InitialInputRequest{Kind: InitialInputArgument, Value: text, SHA256: "sha256:" + hex.EncodeToString(sum[:])}
			conversation := ConversationIdentity{Provider: provider, ID: testConversationID}
			var plan AgentPlan
			var err error
			switch provider {
			case ClaudeProvider:
				plan, err = NewClaudeAdapter(executable).PlanResume(ResumeRequest{PlanRequest: request, Conversation: conversation})
			case CodexProvider:
				plan, err = NewCodexAdapter(executable).PlanResume(ResumeRequest{PlanRequest: request, Conversation: conversation})
			case GrokProvider:
				plan, err = NewGrokAdapter(executable).PlanResume(ResumeRequest{PlanRequest: request, Conversation: conversation})
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.InitialInput != nil || slices.Contains(plan.Argv, text) {
				t.Fatalf("resume redelivered initial input: %#v", plan)
			}
			if provider == ClaudeProvider || provider == GrokProvider {
				if !slices.Equal(plan.Argv[len(plan.Argv)-2:], []string{"--resume", testConversationID}) {
					t.Fatalf("%s resume tail = %q", provider, plan.Argv)
				}
			} else if plan.Argv[len(plan.Argv)-1] != testConversationID {
				t.Fatalf("Codex resume identity is not final: %q", plan.Argv)
			}
		})
	}
}

func TestCodexPlansUseExactCaptureAndResumeShapes(t *testing.T) {
	project, executable := testExecutable(t, CodexProvider)
	adapter := NewCodexAdapter(executable)
	request := planRequest(project, CodexProvider)
	request.CommittedArgs = []string{"--model", "gpt-5.6-sol", "--sandbox", "workspace-write"}
	request.BypassArgs = []string{"--dangerously-bypass-approvals-and-sandbox"}

	fresh, err := adapter.PlanFresh(request)
	if err != nil {
		t.Fatal(err)
	}
	notify, err := codexNotifyOverride(request)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.AdapterMode != AdapterModeCapture || fresh.ConversationID != "" || fresh.PreSpawnAcquire ||
		len(fresh.DynamicArgv) != 0 || !reflect.DeepEqual(fresh.Argv[len(fresh.Argv)-2:], []string{"-c", notify}) {
		t.Fatalf("fresh plan = %#v", fresh)
	}
	otherRequest := request
	otherRequest.LaunchNonce = "56565656-5656-4565-8565-565656565656"
	otherFresh, err := adapter.PlanFresh(otherRequest)
	if err != nil {
		t.Fatal(err)
	}
	freshDigest, err := (Plan{Version: PlanVersion, Agents: []AgentPlan{fresh}}).SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	otherDigest, err := (Plan{Version: PlanVersion, Agents: []AgentPlan{otherFresh}}).SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if freshDigest != otherDigest || fresh.Argv[len(fresh.Argv)-1] != otherFresh.Argv[len(otherFresh.Argv)-1] {
		t.Fatalf("codex notify changed the static trust subject across launch nonces: %s != %s", freshDigest, otherDigest)
	}
	resume, err := adapter.PlanResume(ResumeRequest{
		PlanRequest:  request,
		Conversation: ConversationIdentity{Provider: CodexProvider, ID: testConversationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Argv) < 3 || resume.Argv[1] != "resume" || resume.Argv[len(resume.Argv)-1] != testConversationID {
		t.Fatalf("resume argv = %q", resume.Argv)
	}
	for _, arg := range resume.Argv {
		if arg == "--last" {
			t.Fatalf("resume argv used forbidden heuristic: %q", resume.Argv)
		}
	}
}

func TestAdapterEnvAndArgGrammarFailsClosed(t *testing.T) {
	project, executable := testExecutable(t, ClaudeProvider)
	adapter := NewClaudeAdapter(executable)
	base := planRequest(project, ClaudeProvider)
	tests := []struct {
		name   string
		mutate func(*PlanRequest)
		want   string
	}{
		{"node options", func(request *PlanRequest) { request.EnvOverlay["NODE_OPTIONS"] = "--require=./repo.js" }, "NODE_OPTIONS"},
		{"path", func(request *PlanRequest) { request.EnvOverlay["PATH"] = filepath.Join(project, "bin") }, "PATH"},
		{"config redirect", func(request *PlanRequest) { request.EnvOverlay["CLAUDE_CONFIG_DIR"] = project }, "CLAUDE_CONFIG_DIR"},
		{"literal interpolation", func(request *PlanRequest) { request.EnvOverlay["LANG"] = "${REPO_LANG}" }, "invalid literal"},
		{"shell wrapper", func(request *PlanRequest) { request.CommittedArgs = []string{"bash", "./wrapper.sh"} }, "not allowed"},
		{"identity override", func(request *PlanRequest) { request.CommittedArgs = []string{"--session-id", testConversationID} }, "not allowed"},
		{"committed bypass", func(request *PlanRequest) { request.CommittedArgs = []string{"--permission-mode", "bypassPermissions"} }, "invalid value"},
		{"bypass as flag value", func(request *PlanRequest) {
			request.CommittedArgs = []string{"--model", "--dangerously-skip-permissions"}
		}, "invalid value"},
		{"unknown bypass", func(request *PlanRequest) { request.BypassArgs = []string{"--allow-everything"} }, "unrecognized operator bypass"},
		{"cwd escape", func(request *PlanRequest) { request.Cwd = t.TempDir() }, "inside the project"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.EnvOverlay = cloneEnv(base.EnvOverlay)
			test.mutate(&request)
			if _, err := adapter.PlanFresh(request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("PlanFresh error = %v, want %q", err, test.want)
			}
		})
	}
	t.Run("codex committed notify override", func(t *testing.T) {
		codexProject, codexExecutable := testExecutable(t, CodexProvider)
		request := planRequest(codexProject, CodexProvider)
		request.CommittedArgs = []string{"-c", "notify=[]"}
		if _, err := NewCodexAdapter(codexExecutable).PlanFresh(request); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("PlanFresh error = %v, want committed notify refusal", err)
		}
	})
}

func TestAdapterRejectsProjectExecutableAndProviderMismatch(t *testing.T) {
	project := t.TempDir()
	inside := filepath.Join(project, ClaudeProvider)
	if err := os.WriteFile(inside, []byte("not executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	adapter := NewClaudeAdapter(inside)
	request := planRequest(project, ClaudeProvider)
	if _, err := adapter.PlanFresh(request); err == nil || !strings.Contains(err.Error(), "inside the project") {
		t.Fatalf("project executable error = %v", err)
	}
	externalLink := filepath.Join(t.TempDir(), ClaudeProvider)
	if err := os.Symlink(inside, externalLink); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClaudeAdapter(externalLink).PlanFresh(request); err == nil || !strings.Contains(err.Error(), "inside the project") {
		t.Fatalf("external symlink to project executable error = %v", err)
	}

	otherProject, codexExecutable := testExecutable(t, CodexProvider)
	request = planRequest(otherProject, ClaudeProvider)
	if _, err := NewClaudeAdapter(codexExecutable).PlanFresh(request); err == nil || !strings.Contains(err.Error(), "cannot execute") {
		t.Fatalf("provider executable error = %v", err)
	}
}

func TestAdapterRejectsProjectTrackedProviderSymlinkBeforeSentinelExec(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	marker := filepath.Join(t.TempDir(), "provider-ran")
	provider := filepath.Join(outside, ClaudeProvider)
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf ran > \"$AMQ_SENTINEL\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(project, ClaudeProvider)
	if err := os.Symlink(provider, tracked); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMQ_SENTINEL", marker)
	request := planRequest(project, ClaudeProvider)
	plan, err := NewClaudeAdapter(tracked).PlanFresh(request)
	if err == nil {
		_ = exec.Command(plan.Argv[0]).Run()
		t.Fatal("project-tracked provider symlink was accepted")
	}
	if !strings.Contains(err.Error(), providerProjectContainedCode) {
		t.Fatalf("project-tracked provider error = %v, want %s", err, providerProjectContainedCode)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("provider sentinel was executed: %v", statErr)
	}
}

func TestAdapterKeepsStableExecutableAcrossSymlinkRetarget(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	bin := filepath.Join(base, "bin")
	versions := filepath.Join(base, "versions")
	for _, dir := range []string{project, bin, versions} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	versionA := filepath.Join(versions, "claude-a")
	versionB := filepath.Join(versions, "claude-b")
	for _, path := range []string{versionA, versionB} {
		if err := os.WriteFile(path, []byte(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(bin, ClaudeProvider)
	if err := os.Symlink(versionA, executable); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	request := planRequest(project, ClaudeProvider)
	first, err := NewClaudeAdapter(ClaudeProvider).PlanFresh(request)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := (Plan{Version: PlanVersion, Agents: []AgentPlan{first}}).SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versionB, executable); err != nil {
		t.Fatal(err)
	}
	second, err := NewClaudeAdapter(ClaudeProvider).PlanFresh(request)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := (Plan{Version: PlanVersion, Agents: []AgentPlan{second}}).SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if first.Argv[0] != executable || second.Argv[0] != executable {
		t.Fatalf("planned argv0 = %q then %q, want stable %q", first.Argv[0], second.Argv[0], executable)
	}
	if firstDigest != secondDigest {
		t.Fatalf("symlink retarget changed semantic digest: %s != %s", firstDigest, secondDigest)
	}
	_, root := openTestRoot(t)
	emitted, err := (Commands{}).Create(CreateRequest{
		Session: "collab", AMQPath: "amq", Plan: Plan{Version: PlanVersion, Agents: []AgentPlan{second}}, Root: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(emitted.Commands) != 1 || len(emitted.Commands[0].Argv) < 8 || emitted.Commands[0].Argv[7] != executable {
		t.Fatalf("emitted command argv = %#v, want stable executable %q", emitted.Commands, executable)
	}
}

type misdeclaredAdapter struct{}

func (misdeclaredAdapter) Name() string               { return "bad" }
func (misdeclaredAdapter) Mode() AdapterMode          { return AdapterModeMint }
func (misdeclaredAdapter) CommittedEnvKeys() []string { return nil }
func (misdeclaredAdapter) Capabilities(context.Context) AdapterCapabilities {
	return AdapterCapabilities{}
}
func (misdeclaredAdapter) PlanFresh(PlanRequest) (AgentPlan, error) {
	return AgentPlan{}, errors.New("unused")
}
func (misdeclaredAdapter) PlanResume(ResumeRequest) (AgentPlan, error) {
	return AgentPlan{}, errors.New("unused")
}
func (misdeclaredAdapter) CaptureIdentity(CaptureRequest) CaptureResult { return CaptureResult{} }

func TestValidateAdapterPlanRejectsModeMisdeclaration(t *testing.T) {
	plan := AgentPlan{
		Handle: "bad", Argv: []string{"/bin/bad"}, Cwd: "/tmp",
		AdapterMode: AdapterModeCapture, ResumePolicy: ResumeEnabled,
	}
	if err := ValidateAdapterPlan(misdeclaredAdapter{}, plan); err == nil || !strings.Contains(err.Error(), "declares") {
		t.Fatalf("mode declaration error = %v", err)
	}
}

func TestValidateAdapterCapabilitiesRejectsModeMisdeclaration(t *testing.T) {
	adapter := misdeclaredAdapter{}
	tests := []AdapterCapabilities{
		{Provider: "other", Mode: AdapterModeMint},
		{Provider: "bad", Mode: AdapterModeCapture},
		{Provider: "bad", Mode: AdapterModeMint, Available: true, Capture: true},
	}
	for _, capabilities := range tests {
		if err := ValidateAdapterCapabilities(adapter, capabilities); err == nil {
			t.Fatalf("ValidateAdapterCapabilities(%#v) error = nil", capabilities)
		}
	}
}

func TestCommittedConfigValidationDoesNotRequireInstalledExecutable(t *testing.T) {
	project := t.TempDir()
	adapter := NewClaudeAdapter("definitely-not-installed-claude")
	if err := ValidateCommittedConfig(adapter, CommittedConfigRequest{
		ProjectRoot: project,
		Args:        []string{"--no-chrome"},
		EnvOverlay:  map[string]string{"TERM": "xterm-256color"},
	}); err != nil {
		t.Fatalf("safe committed config: %v", err)
	}
	for _, test := range []struct {
		name    string
		request CommittedConfigRequest
		want    string
	}{
		{
			name: "loader environment",
			request: CommittedConfigRequest{ProjectRoot: project,
				EnvOverlay: map[string]string{"NODE_OPTIONS": "--require ./evil.js"}},
			want: "NODE_OPTIONS",
		},
		{
			name:    "wrapper argv",
			request: CommittedConfigRequest{ProjectRoot: project, Args: []string{"bash", "./agent-wrapper"}},
			want:    "bash",
		},
		{
			name:    "cwd escape",
			request: CommittedConfigRequest{ProjectRoot: project, Cwd: filepath.Dir(project)},
			want:    "inside the project",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCommittedConfig(adapter, test.request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
