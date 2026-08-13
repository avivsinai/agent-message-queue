package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

	codex := NewCodexAdapter("codex")
	codex.probe = fakeCommandProbe{path: "/bin/codex", outputs: map[string]string{
		"--version":         "codex-cli 0.147.0\n",
		"--help":            "commands: resume",
		"resume --help":     "Usage: codex resume [OPTIONS] [SESSION_ID]",
		"app-server --help": "generate-json-schema",
	}}
	gotCodex := codex.Capabilities(context.Background())
	if !gotCodex.Available || !gotCodex.Fresh || !gotCodex.Resume || !gotCodex.Capture || gotCodex.Mode != AdapterModeCapture {
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
		"resume --help": "--last only", "app-server --help": "generate-json-schema",
	}}
	unsupported := codex.Capabilities(context.Background())
	if unsupported.Available || unsupported.Reason != "identity_contract_unsupported" {
		t.Fatalf("unsupported Codex capabilities = %#v", unsupported)
	}
	codex.probe = fakeCommandProbe{path: "/bin/codex", outputs: map[string]string{
		"--version": "codex-cli 0.148.0\n", "--help": "commands: resume",
		"resume --help": "Usage: codex resume [OPTIONS] [SESSION_ID]", "app-server --help": "generate-json-schema",
	}}
	untestedVersion := codex.Capabilities(context.Background())
	if !untestedVersion.Available || !untestedVersion.Fresh || !untestedVersion.Resume || untestedVersion.Capture || untestedVersion.Reason != "capture_version_unsupported" {
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
	if fresh.AdapterMode != AdapterModeCapture || fresh.ConversationID != "" || len(fresh.DynamicArgv) != 0 {
		t.Fatalf("fresh plan = %#v", fresh)
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

	otherProject, codexExecutable := testExecutable(t, CodexProvider)
	request = planRequest(otherProject, ClaudeProvider)
	if _, err := NewClaudeAdapter(codexExecutable).PlanFresh(request); err == nil || !strings.Contains(err.Error(), "cannot execute") {
		t.Fatalf("provider executable error = %v", err)
	}
}

func TestAdapterResolvesConfiguredExecutableFromPath(t *testing.T) {
	project, executable := testExecutable(t, ClaudeProvider)
	t.Setenv("PATH", filepath.Dir(executable))
	plan, err := NewClaudeAdapter(ClaudeProvider).PlanFresh(planRequest(project, ClaudeProvider))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Argv[0] != resolved {
		t.Fatalf("resolved executable = %q, want %q", plan.Argv[0], resolved)
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
