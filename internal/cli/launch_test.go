package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

type launchFixtureAdapter struct {
	available bool
	reason    string
}

func (launchFixtureAdapter) Name() string               { return launch.ClaudeProvider }
func (launchFixtureAdapter) Mode() launch.AdapterMode   { return launch.AdapterModeMint }
func (launchFixtureAdapter) CommittedEnvKeys() []string { return nil }
func (a launchFixtureAdapter) Capabilities(context.Context) launch.AdapterCapabilities {
	return launch.AdapterCapabilities{
		Provider: launch.ClaudeProvider, Mode: launch.AdapterModeMint, Available: a.available,
		ProviderVersion: "test", Fresh: a.available, Resume: a.available, Reason: a.reason,
	}
}
func (launchFixtureAdapter) PlanFresh(req launch.PlanRequest) (launch.AgentPlan, error) {
	return launch.AgentPlan{
		Handle: req.Handle, Argv: []string{"/usr/bin/true", req.LaunchNonce}, Cwd: req.Cwd,
		AdapterMode: launch.AdapterModeMint, ResumePolicy: req.ResumePolicy,
		LaunchNonce: req.LaunchNonce, ConversationID: req.LaunchNonce,
		DynamicArgv: []launch.DynamicArg{{Index: 1, Kind: launch.DynamicArgLaunchNonce}},
	}, nil
}
func (launchFixtureAdapter) PlanResume(req launch.ResumeRequest) (launch.AgentPlan, error) {
	return launch.AgentPlan{
		Handle: req.Handle, Argv: []string{"/usr/bin/true", req.Conversation.ID}, Cwd: req.Cwd,
		AdapterMode: launch.AdapterModeMint, ResumePolicy: launch.ResumeEnabled,
		LaunchNonce: req.LaunchNonce, ConversationID: req.Conversation.ID,
		DynamicArgv: []launch.DynamicArg{{Index: 1, Kind: launch.DynamicArgConversationID}},
	}, nil
}
func (launchFixtureAdapter) CaptureIdentity(launch.CaptureRequest) launch.CaptureResult {
	return launch.CaptureResult{State: launch.CaptureUnsupported, Reason: launch.CaptureReasonAdapterMintsIdentity}
}

func launchCLIFixture(t *testing.T, sessions ...string) (string, string) {
	t.Helper()
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{envRoot, envBaseRoot, envRootID, envBaseRootID, envSession, envGlobalRoot} {
		value, present := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectConfig := launch.ProjectConfig{
		Schema: launch.ProjectConfigSchema, DefaultSession: sessions[0], Layout: launch.LayoutIntent{Type: launch.LayoutColumns},
		Agents: []launch.ProjectAgentConfig{{Handle: "claude", Adapter: "claude", Command: []string{"claude"}, ResumePolicy: launch.ResumeEnabled}},
	}
	projectData, err := launch.MarshalProjectConfig(projectConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, setupConfigPath), projectData, 0o600); err != nil {
		t.Fatal(err)
	}
	localData, err := launch.MarshalLocalConfig(launch.LocalConfig{Schema: launch.LocalConfigSchema, LauncherPreference: []string{launch.LauncherCommands}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, setupLocalConfigPath), localData, 0o600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(project, defaultCoopRoot)
	if err := fsq.EnsureRootDirs(base); err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		root := filepath.Join(base, session)
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
			t.Fatal(err)
		}
	}
	state := t.TempDir()
	launchIsTerminal = func() bool { return false }
	launchStateDir = func() (string, error) { return state, nil }
	launchAMQPath = func() string { return "amq" }
	launchAdapters = func(launch.ProjectConfig) map[string]launch.HarnessAdapter {
		return map[string]launch.HarnessAdapter{"claude": launchFixtureAdapter{available: true}}
	}
	launchBackends = func() map[string]launch.Backend {
		return map[string]launch.Backend{launch.LauncherCommands: launch.Commands{}}
	}
	launchHostname = func() (string, error) { return "host:test", nil }
	t.Cleanup(func() {
		_ = os.Chdir(oldCWD)
		launchIsTerminal = func() bool { return false }
		launchInput = func() *bufio.Reader { return bufio.NewReader(os.Stdin) }
		launchStateDir = defaultLaunchStateDir
		launchAMQPath = func() string { return "amq" }
		launchAdapters = defaultLaunchAdapters
		launchBackends = func() map[string]launch.Backend {
			return map[string]launch.Backend{launch.LauncherCommands: launch.Commands{}}
		}
		launchHostname = os.Hostname
	})
	return project, state
}

func TestLaunchNonInteractiveUntrustedIsExit6AndNoRuntimeWrites(t *testing.T) {
	project, _ := launchCLIFixture(t, "collab")
	root := filepath.Join(project, defaultCoopRoot, "collab")
	stdout, _, err := captureEnvOutput(t, func() error { return runLaunch([]string{"--json"}) })
	if GetExitCode(err) != ExitActionRequired {
		t.Fatalf("exit=%d err=%v output=%s", GetExitCode(err), err, stdout)
	}
	var result launch.ReconcileResult
	if json.Unmarshal([]byte(stdout), &result) != nil || result.AggregateCode != ExitActionRequired || result.Reason != "launch plan requires local trust confirmation" {
		t.Fatalf("result=%s", stdout)
	}
	if _, err := os.Stat(launch.ConversationPath(root, "claude")); !os.IsNotExist(err) {
		t.Fatalf("untrusted launch wrote conversation state: %v", err)
	}
}

func TestLaunchWithoutExecutionRemintsAndResumeJSONKeepsSchema(t *testing.T) {
	project, _ := launchCLIFixture(t, "collab", "empty")
	launchIsTerminal = func() bool { return true }
	launchInput = func() *bufio.Reader { return bufio.NewReader(strings.NewReader("y\n")) }
	if _, _, err := captureEnvOutput(t, func() error { return runLaunch([]string{"--session", "collab"}) }); GetExitCode(err) != ExitActionRequired {
		t.Fatalf("first launch exit=%d err=%v", GetExitCode(err), err)
	}
	loadRecord := func(session string) launch.ConversationRecord {
		data, err := os.ReadFile(launch.ConversationPath(filepath.Join(project, defaultCoopRoot, session), "claude"))
		if err != nil {
			t.Fatal(err)
		}
		var record launch.ConversationRecord
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	first := loadRecord("collab")
	if first.State != launch.CapturePending || first.Identity.ID != "" || first.ExecutionEvidence != nil {
		t.Fatalf("first record=%#v", first)
	}

	launchJSON, _, err := captureEnvOutput(t, func() error { return runLaunch([]string{"--session", "collab", "--json"}) })
	if GetExitCode(err) != ExitActionRequired {
		t.Fatalf("second launch exit=%d output=%s err=%v", GetExitCode(err), launchJSON, err)
	}
	var second launch.ReconcileResult
	if err := json.Unmarshal([]byte(launchJSON), &second); err != nil {
		t.Fatal(err)
	}
	if second.Plan == nil || second.Plan.Agents[0].ConversationID == first.LaunchNonce ||
		second.Agents[0].ConversationDisposition != launch.DispositionFresh || second.Agents[0].Reason != launch.ReasonPriorLaunchNotExecuted {
		t.Fatalf("second result=%#v first=%#v", second, first)
	}
	secondRecord := loadRecord("collab")
	if secondRecord.State != launch.CapturePending || secondRecord.LaunchNonce != second.Plan.Agents[0].ConversationID || secondRecord.ExecutionEvidence != nil {
		t.Fatalf("second record=%#v result=%#v", secondRecord, second)
	}

	resumeJSON, _, err := captureEnvOutput(t, func() error { return runSession([]string{"resume", "empty", "--json"}) })
	if GetExitCode(err) != ExitActionRequired {
		t.Fatalf("empty resume exit=%d output=%s err=%v", GetExitCode(err), resumeJSON, err)
	}
	var resume launch.ReconcileResult
	if err := json.Unmarshal([]byte(resumeJSON), &resume); err != nil {
		t.Fatal(err)
	}
	if resume.Outcome != launch.OutcomeActionRequired || resume.Reason != launch.ReasonNoSavedConversation ||
		resume.Agents[0].ConversationDisposition != launch.DispositionActionRequired {
		t.Fatalf("resume result=%#v", resume)
	}
	var launchShape, resumeShape map[string]json.RawMessage
	if err := json.Unmarshal([]byte(launchJSON), &launchShape); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(resumeJSON), &resumeShape); err != nil {
		t.Fatal(err)
	}
	launchKeys, resumeKeys := map[string]bool{}, map[string]bool{}
	for key := range launchShape {
		launchKeys[key] = true
	}
	for key := range resumeShape {
		resumeKeys[key] = true
	}
	if !reflect.DeepEqual(launchKeys, resumeKeys) {
		t.Fatalf("launch JSON keys=%v resume JSON keys=%v", launchKeys, resumeKeys)
	}
}

func TestLaunchMissingBinaryIsStructuredDisposition(t *testing.T) {
	launchCLIFixture(t, "collab")
	launchAdapters = func(launch.ProjectConfig) map[string]launch.HarnessAdapter {
		return map[string]launch.HarnessAdapter{"claude": launchFixtureAdapter{reason: "executable_not_found"}}
	}
	stdout, _, err := captureEnvOutput(t, func() error { return runLaunch([]string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var result launch.ReconcileResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Agents) != 1 || result.Agents[0].ConversationDisposition != launch.DispositionUnsupported || result.Agents[0].Reason != "executable_not_found" {
		t.Fatalf("result=%#v", result)
	}
}

func TestSessionResumeHelpAndInvalidNameAreUsageSafe(t *testing.T) {
	stdout, _, err := captureEnvOutput(t, func() error { return runSession([]string{"resume", "--help"}) })
	if err != nil || !strings.Contains(stdout, "amq session resume <name>") {
		t.Fatalf("help output=%q err=%v", stdout, err)
	}
	if err := runSession([]string{"resume", "Bad.Name"}); GetExitCode(err) != ExitUsage {
		t.Fatalf("invalid name exit=%d err=%v", GetExitCode(err), err)
	}
}
