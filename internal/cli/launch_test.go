package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

type launchFixtureAdapter struct {
	name               string
	mode               launch.AdapterMode
	available          bool
	captureUnsupported bool
	reason             string
}

func (a launchFixtureAdapter) Name() string {
	if a.name != "" {
		return a.name
	}
	return launch.ClaudeProvider
}
func (a launchFixtureAdapter) Mode() launch.AdapterMode {
	if a.mode != "" {
		return a.mode
	}
	return launch.AdapterModeMint
}
func (launchFixtureAdapter) CommittedEnvKeys() []string { return nil }
func (a launchFixtureAdapter) Capabilities(context.Context) launch.AdapterCapabilities {
	return launch.AdapterCapabilities{
		Provider: a.Name(), Mode: a.Mode(), Available: a.available,
		ProviderVersion: "test", Fresh: a.available, Resume: a.available,
		Capture: a.available && a.Mode() == launch.AdapterModeCapture && !a.captureUnsupported, Reason: a.reason,
	}
}
func (a launchFixtureAdapter) PlanFresh(req launch.PlanRequest) (launch.AgentPlan, error) {
	return launch.AgentPlan{
		Handle: req.Handle, Argv: []string{"/usr/bin/true", req.LaunchNonce}, Cwd: req.Cwd,
		AdapterMode: a.Mode(), ResumePolicy: req.ResumePolicy,
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
	launchAMQPath = func() string { path, _ := os.Executable(); return path }
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
		launchAMQPath = func() string { path, _ := os.Executable(); return path }
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
