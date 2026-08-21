package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

type setupFixtureAdapter struct{ name string }

type setupValidatingFixtureAdapter struct {
	setupFixtureAdapter
	validator launch.CommittedConfigValidator
}

func (a setupValidatingFixtureAdapter) ValidateCommittedConfig(request launch.CommittedConfigRequest) error {
	return a.validator.ValidateCommittedConfig(request)
}

func (a setupFixtureAdapter) Name() string               { return a.name }
func (a setupFixtureAdapter) Mode() launch.AdapterMode   { return launch.AdapterModeMint }
func (a setupFixtureAdapter) CommittedEnvKeys() []string { return nil }
func (a setupFixtureAdapter) Capabilities(context.Context) launch.AdapterCapabilities {
	return launch.AdapterCapabilities{
		Provider: a.name, Mode: a.Mode(), Available: true, Executable: "/outside/" + a.name,
		ProviderVersion: "test", Fresh: true, Resume: true,
	}
}
func (a setupFixtureAdapter) PlanFresh(launch.PlanRequest) (launch.AgentPlan, error) {
	return launch.AgentPlan{}, errors.New("not used")
}
func (a setupFixtureAdapter) PlanResume(launch.ResumeRequest) (launch.AgentPlan, error) {
	return launch.AgentPlan{}, errors.New("not used")
}
func (a setupFixtureAdapter) CaptureIdentity(launch.CaptureRequest) launch.CaptureResult {
	return launch.CaptureResult{State: launch.CaptureUnsupported}
}
func (a setupFixtureAdapter) ValidateCommittedConfig(launch.CommittedConfigRequest) error {
	return nil
}

func TestSetupWritesAuthoritativeAndPreferenceScopesThenNoOps(t *testing.T) {
	project := setupProjectFixture(t, "claude", "codex", "grok")
	const gitignoreBefore = "# user-owned bytes\ncustom/cache\n"
	if err := os.WriteFile(filepath.Join(project, ".gitignore"), []byte(gitignoreBefore), 0o644); err != nil {
		t.Fatal(err)
	}
	setupLookPath = func(name string) (string, error) {
		if name == launch.LauncherTMux {
			return "/usr/bin/tmux", nil
		}
		return "", fs.ErrNotExist
	}
	t.Cleanup(func() { setupLookPath = execLookPathForSetup })

	if _, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude,codex,grok", "--default-session", "work", "--launcher-preference", "tmux", "--json"})
	}); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	projectRaw, err := os.ReadFile(filepath.Join(project, setupConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	projectConfig, err := launch.ParseProjectConfig(projectRaw)
	if err != nil {
		t.Fatal(err)
	}
	if got := projectAgentHandles(projectConfig.Agents); !slices.Equal(got, []string{"claude", "codex", "grok"}) {
		t.Fatalf("roster = %v", got)
	}
	if projectConfig.DefaultSession != "work" {
		t.Fatalf("default session = %q", projectConfig.DefaultSession)
	}
	localRaw, err := os.ReadFile(filepath.Join(project, setupLocalConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	local, err := launch.ParseLocalConfig(setupLocalConfigPath, localRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(local.LauncherPreference, []string{launch.LauncherTMux, launch.LauncherCommands}) {
		t.Fatalf("launcher preference = %v", local.LauncherPreference)
	}
	for _, forbidden := range []string{"default_session", "agents", "argv", "env", "cwd", "bypass_args"} {
		if strings.Contains(string(localRaw), `"`+forbidden+`"`) {
			t.Fatalf("local config contains authority field %q: %s", forbidden, localRaw)
		}
	}
	gitignoreAfter, err := os.ReadFile(filepath.Join(project, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(gitignoreAfter), gitignoreBefore) ||
		!strings.Contains(string(gitignoreAfter), ".agent-mail/\n") ||
		!strings.Contains(string(gitignoreAfter), setupLocalConfigPath+"\n") {
		t.Fatalf("gitignore did not preserve prefix and append entries:\n%s", gitignoreAfter)
	}
	for _, base := range []string{filepath.Join(project, defaultCoopRoot), filepath.Join(project, defaultCoopRoot, "work")} {
		for _, agent := range []string{"claude", "codex", "grok"} {
			if err := validateSetupMailbox(base, agent); err != nil {
				t.Fatalf("mailbox %s/%s: %v", base, agent, err)
			}
		}
	}

	before := setupTreeDigest(t, project)
	writes := 0
	setupCommitStepHook = func(string) error { writes++; return nil }
	t.Cleanup(func() { setupCommitStepHook = nil })
	output, err := captureEnvStdout(t, func() error { return runSetup([]string{"-y", "--json"}) })
	if err != nil {
		t.Fatalf("matching rerun: %v", err)
	}
	if writes != 0 || !strings.Contains(output, `"status": "unchanged"`) {
		t.Fatalf("matching rerun writes=%d output=%s", writes, output)
	}
	after := setupTreeDigest(t, project)
	if before != after {
		t.Fatalf("matching rerun changed tree digest: %x != %x", before, after)
	}
}

func TestSetupCursorCommandPrefersAgentAndExplainsLegacyFallback(t *testing.T) {
	detected := []launch.AdapterCapabilities{{Provider: launch.CursorProvider, Fresh: true, Available: true}}
	options := setupOptions{agents: launch.CursorProvider, agentsExplicit: true, nonInteractive: true}
	setupLookPath = func(name string) (string, error) {
		if name == "agent" {
			return "/opt/cursor/agent", nil
		}
		return "", fs.ErrNotExist
	}
	t.Cleanup(func() { setupLookPath = execLookPathForSetup })
	agents, err := chooseSetupAgents(options, detected, launch.ProjectConfig{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || !slices.Equal(agents[0].Command, []string{"agent"}) || agents[0].Adapter != launch.CursorProvider {
		t.Fatalf("preferred Cursor setup agent = %#v", agents)
	}
	if notes := setupAgentExplanations(agents); len(notes) != 0 {
		t.Fatalf("preferred Cursor setup explanation = %#v", notes)
	}

	setupLookPath = func(string) (string, error) { return "", fs.ErrNotExist }
	agents, err = chooseSetupAgents(options, detected, launch.ProjectConfig{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || !slices.Equal(agents[0].Command, []string{"cursor-agent"}) {
		t.Fatalf("legacy Cursor setup fallback = %#v", agents)
	}
	notes := setupAgentExplanations(agents)
	if len(notes) != 1 || !strings.Contains(notes[0], "not found") {
		t.Fatalf("legacy Cursor setup explanation = %#v", notes)
	}
}

func TestSetupPreviewDigestBindsApplyWithoutWrites(t *testing.T) {
	project := setupProjectFixture(t, "claude", "codex")
	args := []string{
		"--agents", "claude,codex", "--default-session", "collab",
		"--launcher-preference", "commands",
	}
	before := setupTreeDigest(t, project)
	steps := 0
	setupCommitStepHook = func(string) error { steps++; return nil }

	previewOutput, err := captureEnvStdout(t, func() error {
		return runSetup(append([]string{"--preview", "--json"}, args...))
	})
	if err != nil {
		t.Fatal(err)
	}
	var previewResult setupResult
	if err := json.Unmarshal([]byte(previewOutput), &previewResult); err != nil {
		t.Fatal(err)
	}
	if previewResult.Status != "preview" || !validSetupDigest(previewResult.Preview.Digest) || len(previewResult.Written) != 0 {
		t.Fatalf("preview result=%#v", previewResult)
	}
	if steps != 0 || before != setupTreeDigest(t, project) {
		t.Fatalf("preview steps=%d changed=%t", steps, before != setupTreeDigest(t, project))
	}

	textOutput, err := captureEnvStdout(t, func() error {
		return runSetup(append([]string{"--preview"}, args...))
	})
	if err != nil || !strings.Contains(textOutput, "Approval digest: "+previewResult.Preview.Digest) {
		t.Fatalf("text preview output=%q err=%v", textOutput, err)
	}
	if steps != 0 || before != setupTreeDigest(t, project) {
		t.Fatalf("text preview steps=%d changed=%t", steps, before != setupTreeDigest(t, project))
	}

	mismatchArgs := append([]string{"--apply", previewResult.Preview.Digest}, args...)
	for i, value := range mismatchArgs {
		if value == "collab" {
			mismatchArgs[i] = "other"
			break
		}
	}
	_, err = captureEnvStdout(t, func() error { return runSetup(mismatchArgs) })
	if GetExitCode(err) != ExitActionRequired || steps != 0 || before != setupTreeDigest(t, project) {
		t.Fatalf("mismatch exit=%d err=%v steps=%d changed=%t", GetExitCode(err), err, steps, before != setupTreeDigest(t, project))
	}

	applyOutput, err := captureEnvStdout(t, func() error {
		return runSetup(append([]string{"--apply", previewResult.Preview.Digest}, args...))
	})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, applyOutput)
	}
	if !strings.Contains(applyOutput, "Setup committed") || steps == 0 {
		t.Fatalf("apply output=%q steps=%d", applyOutput, steps)
	}
	assertCompleteSetup(t, project, "claude", "codex")
}

func TestSetupPreviewAndApplyAcceptanceModesAreExclusive(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	digest := "sha256:" + strings.Repeat("0", 64)
	for _, args := range [][]string{
		{"--preview", "-y"},
		{"--apply", digest, "-y"},
		{"--preview", "--apply", digest},
		{"--apply", "not-a-digest"},
	} {
		if err := runSetup(args); GetExitCode(err) != ExitUsage {
			t.Fatalf("args=%v exit=%d err=%v", args, GetExitCode(err), err)
		}
	}
	if entries, err := os.ReadDir(project); err != nil || len(entries) != 0 {
		t.Fatalf("usage refusals wrote project entries=%v err=%v", entries, err)
	}
}

func TestSetupFirstNonInteractiveRunRequiresExplicitSemanticInputs(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	before := setupTreeDigest(t, project)
	_, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"--preview", "--json", "--agents", "claude"})
	})
	if GetExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "--default-session") ||
		!strings.Contains(err.Error(), "--launcher-preference") {
		t.Fatalf("explicit-input refusal exit=%d err=%v", GetExitCode(err), err)
	}
	if before != setupTreeDigest(t, project) {
		t.Fatal("explicit-input refusal changed project")
	}
}

func TestSetupInterruptionPrefixesConverge(t *testing.T) {
	steps := []string{"provision", "roster_compatible", "project_config", "local_config", "gitignore", "amqrc"}
	for _, stop := range steps {
		t.Run(stop, func(t *testing.T) {
			project := setupProjectFixture(t, "claude")
			injected := errors.New("injected after " + stop)
			setupCommitStepHook = func(step string) error {
				if step == stop {
					return injected
				}
				return nil
			}
			_, err := captureEnvStdout(t, func() error {
				return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands"})
			})
			if !errors.Is(err, injected) {
				t.Fatalf("interrupted setup error = %v", err)
			}
			assertSetupPrefixValid(t, project)
			setupCommitStepHook = nil
			if _, err := captureEnvStdout(t, func() error {
				return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands"})
			}); err != nil {
				t.Fatalf("recovery rerun: %v", err)
			}
			assertCompleteSetup(t, project, "claude")
			before := setupTreeDigest(t, project)
			calls := 0
			setupCommitStepHook = func(string) error { calls++; return nil }
			if _, err := captureEnvStdout(t, func() error { return runSetup([]string{"-y", "--agents", "claude"}) }); err != nil {
				t.Fatalf("stable rerun: %v", err)
			}
			if calls != 0 || before != setupTreeDigest(t, project) {
				t.Fatalf("stable rerun calls=%d changed=%t", calls, before != setupTreeDigest(t, project))
			}
			setupCommitStepHook = nil
		})
	}
}

func TestSetupRefusesLocalAuthorityBeforeWrites(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	if err := os.MkdirAll(filepath.Join(project, ".amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	malicious := []byte(`{"schema":1,"launcher_preference":["commands"],"default_session":"attacker"}`)
	if err := os.WriteFile(filepath.Join(project, setupLocalConfigPath), malicious, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := captureEnvStdout(t, func() error { return runSetup([]string{"-y", "--agents", "claude"}) })
	var conflict *launch.ConfigAuthorityConflictError
	if !errors.As(err, &conflict) || GetExitCode(err) != ExitError {
		t.Fatalf("authority error = %T %v, exit=%d", err, err, GetExitCode(err))
	}
	if _, statErr := os.Lstat(filepath.Join(project, ".amqrc")); !os.IsNotExist(statErr) {
		t.Fatalf("authority refusal wrote .amqrc: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(project, defaultCoopRoot)); !os.IsNotExist(statErr) {
		t.Fatalf("authority refusal provisioned root: %v", statErr)
	}
}

func TestSetupRefusesAdapterHostileCommittedConfigWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(*launch.ProjectAgentConfig)
		wantDetail string
	}{
		{
			name: "loader environment",
			mutate: func(agent *launch.ProjectAgentConfig) {
				agent.Env = map[string]string{"NODE_OPTIONS": "--require ./evil.js"}
			},
			wantDetail: "NODE_OPTIONS",
		},
		{
			name: "repo relative wrapper",
			mutate: func(agent *launch.ProjectAgentConfig) {
				agent.Command = append(agent.Command, "bash", "./agent-wrapper")
			},
			wantDetail: "bash",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := setupProjectFixture(t, "claude")
			setupHarnessAdapters = func() []launch.HarnessAdapter {
				return []launch.HarnessAdapter{setupValidatingFixtureAdapter{
					setupFixtureAdapter: setupFixtureAdapter{name: "claude"},
					validator:           launch.NewClaudeAdapter("claude"),
				}}
			}
			if _, err := captureEnvStdout(t, func() error {
				return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands"})
			}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(project, setupConfigPath)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := launch.ParseProjectConfig(raw)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&cfg.Agents[0])
			hostile, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, hostile, 0o644); err != nil {
				t.Fatal(err)
			}
			before := setupTreeDigest(t, project)
			steps := 0
			setupCommitStepHook = func(string) error { steps++; return nil }
			_, err = captureEnvStdout(t, func() error { return runSetup([]string{"-y"}) })
			var refusal *setupConfigRefusalError
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("hostile config error = %T %v", err, err)
			}
			if steps != 0 || before != setupTreeDigest(t, project) {
				t.Fatalf("hostile refusal steps=%d changed=%t", steps, before != setupTreeDigest(t, project))
			}
		})
	}
}

func TestSetupPreservesExternallyEditedValidConfigAsZeroWrite(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	setupHarnessAdapters = func() []launch.HarnessAdapter {
		return []launch.HarnessAdapter{setupValidatingFixtureAdapter{
			setupFixtureAdapter: setupFixtureAdapter{name: "claude"},
			validator:           launch.NewClaudeAdapter("claude"),
		}}
	}
	if _, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands"})
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, setupConfigPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := launch.ParseProjectConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents[0].Command = append(cfg.Agents[0].Command, "--no-chrome")
	valid, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	before := setupTreeDigest(t, project)
	steps := 0
	setupCommitStepHook = func(string) error { steps++; return nil }
	output, err := captureEnvStdout(t, func() error { return runSetup([]string{"-y", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	if steps != 0 || before != setupTreeDigest(t, project) || !strings.Contains(output, `"status": "unchanged"`) {
		t.Fatalf("valid external edit steps=%d changed=%t output=%s", steps, before != setupTreeDigest(t, project), output)
	}
}

func TestSetupRefusesLegacyDefaultSessionAuthorityInAmqrc(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	legacy := []byte(`{"root":".agent-mail","default_session":"attacker"}`)
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()
	_, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands"})
	})
	var conflict *launch.ConfigAuthorityConflictError
	if !errors.As(err, &conflict) || conflict.Path != ".amqrc" || conflict.Field != "default_session" {
		t.Fatalf("legacy authority error = %T %v", err, err)
	}
	if _, statErr := os.Lstat(filepath.Join(project, defaultCoopRoot)); !os.IsNotExist(statErr) {
		t.Fatalf("authority refusal provisioned root: %v", statErr)
	}
}

func TestSetupRosterReplacementFinalizationRecovers(t *testing.T) {
	for _, stop := range []string{"project_config", "roster_final"} {
		t.Run(stop, func(t *testing.T) {
			project := setupProjectFixture(t, "claude", "codex", "grok")
			if _, err := captureEnvStdout(t, func() error {
				return runSetup([]string{"-y", "--agents", "claude,codex", "--default-session", "collab", "--launcher-preference", "commands"})
			}); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("after " + stop)
			setupCommitStepHook = func(step string) error {
				if step == stop {
					return injected
				}
				return nil
			}
			_, err := captureEnvStdout(t, func() error {
				return runSetup([]string{"-y", "--agents", "claude,grok"})
			})
			if !errors.Is(err, injected) {
				t.Fatalf("roster interruption = %v", err)
			}
			assertSetupPrefixValid(t, project)
			if stop == "project_config" {
				compatible, readErr := os.ReadFile(filepath.Join(project, defaultCoopRoot, "meta", "config.json"))
				if readErr != nil || !strings.Contains(string(compatible), `"codex"`) || !strings.Contains(string(compatible), `"grok"`) {
					t.Fatalf("compatible prefix config = %s err=%v", compatible, readErr)
				}
			}
			setupCommitStepHook = nil
			if _, err := captureEnvStdout(t, func() error {
				return runSetup([]string{"-y", "--agents", "claude,grok"})
			}); err != nil {
				t.Fatalf("roster recovery: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(project, defaultCoopRoot, "meta", "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"codex"`) || !strings.Contains(string(raw), `"grok"`) {
				t.Fatalf("final roster config = %s", raw)
			}
		})
	}
}

func TestSetupInteractiveAbortWritesNothing(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	setupIsTerminal = func() bool { return true }
	t.Cleanup(func() { setupIsTerminal = setupTerminalDefault })
	restore := withStdin(t, "\n\n\nno\n")
	defer restore()
	output, err := captureEnvStdout(t, func() error { return runSetup(nil) })
	if err != nil {
		t.Fatalf("interactive abort: %v", err)
	}
	if !strings.Contains(output, "Changes:") || !strings.Contains(output, "Aborted.") {
		t.Fatalf("output = %q", output)
	}
	for _, path := range []string{".amqrc", defaultCoopRoot, setupConfigPath, setupLocalConfigPath} {
		if _, statErr := os.Lstat(filepath.Join(project, path)); !os.IsNotExist(statErr) {
			t.Fatalf("abort wrote %s: %v", path, statErr)
		}
	}
}

func TestSetupInteractiveRerunCanChangeRosterAndSession(t *testing.T) {
	project := setupProjectFixture(t, "claude", "codex")
	if _, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude,codex", "--default-session", "collab", "--launcher-preference", "commands"})
	}); err != nil {
		t.Fatal(err)
	}
	setupIsTerminal = func() bool { return true }
	restore := withStdin(t, "claude\nreview\ncommands\n\n")
	defer restore()
	if _, err := captureEnvStdout(t, func() error { return runSetup(nil) }); err != nil {
		t.Fatalf("interactive update: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(project, setupConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := launch.ParseProjectConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := projectAgentHandles(cfg.Agents); !slices.Equal(got, []string{"claude"}) || cfg.DefaultSession != "review" {
		t.Fatalf("updated config = %#v", cfg)
	}
	if err := validateSetupMailbox(filepath.Join(project, defaultCoopRoot, "review"), "claude"); err != nil {
		t.Fatalf("updated session mailbox: %v", err)
	}
}

func TestSetupNoGitignorePreservesExistingBytes(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	const before = "keep exactly this without newline"
	if err := os.WriteFile(filepath.Join(project, ".gitignore"), []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"-y", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands", "--no-gitignore"})
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(project, ".gitignore"))
	if err != nil || string(after) != before {
		t.Fatalf("gitignore = %q err=%v", after, err)
	}
}

var (
	execLookPathForSetup         = setupLookPath
	setupCmuxAvailableDefault    = setupCmuxAvailable
	setupGhosttyAvailableDefault = setupGhosttyAvailable
	setupTerminalDefault         = setupIsTerminal
)

func setupProjectFixture(t *testing.T, adapters ...string) string {
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
	resetAmqrcCache()
	setupHarnessAdapters = func() []launch.HarnessAdapter {
		result := make([]launch.HarnessAdapter, 0, len(adapters))
		for _, name := range adapters {
			result = append(result, setupFixtureAdapter{name: name})
		}
		return result
	}
	setupLookPath = func(string) (string, error) { return "", fs.ErrNotExist }
	setupCmuxAvailable = func() bool { return false }
	setupGhosttyAvailable = func() bool { return false }
	setupCommitStepHook = nil
	t.Cleanup(func() {
		_ = os.Chdir(oldCWD)
		resetAmqrcCache()
		setupHarnessAdapters = func() []launch.HarnessAdapter {
			return []launch.HarnessAdapter{
				launch.NewClaudeAdapter(launch.ClaudeProvider),
				launch.NewCodexAdapter(launch.CodexProvider),
				launch.NewCursorAdapter(setupCursorCommand()),
				launch.NewGrokAdapter(launch.GrokProvider),
			}
		}
		setupLookPath = execLookPathForSetup
		setupCmuxAvailable = setupCmuxAvailableDefault
		setupGhosttyAvailable = setupGhosttyAvailableDefault
		setupIsTerminal = setupTerminalDefault
		setupCommitStepHook = nil
	})
	return project
}

func assertSetupPrefixValid(t *testing.T, project string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(project, ".amqrc")); err == nil {
		assertCompleteSetup(t, project, "claude")
	}
	if data, err := os.ReadFile(filepath.Join(project, setupConfigPath)); err == nil {
		cfg, parseErr := launch.ParseProjectConfig(data)
		if parseErr != nil {
			t.Fatalf("partial launch config: %v", parseErr)
		}
		for _, agent := range cfg.Agents {
			if err := validateSetupMailbox(filepath.Join(project, defaultCoopRoot, cfg.DefaultSession), agent.Handle); err != nil {
				t.Fatalf("declared partial roster is not provisioned: %v", err)
			}
		}
	}
}

func assertCompleteSetup(t *testing.T, project string, agents ...string) {
	t.Helper()
	for _, path := range []string{".amqrc", setupConfigPath, setupLocalConfigPath, ".gitignore"} {
		if info, err := os.Stat(filepath.Join(project, path)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("missing setup file %s: info=%v err=%v", path, info, err)
		}
	}
	for _, base := range []string{filepath.Join(project, defaultCoopRoot), filepath.Join(project, defaultCoopRoot, defaultSessionName)} {
		for _, agent := range agents {
			if err := validateSetupMailbox(base, agent); err != nil {
				t.Fatalf("mailbox %s/%s: %v", base, agent, err)
			}
		}
	}
}

func setupTreeDigest(t *testing.T, root string) [32]byte {
	t.Helper()
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00", filepath.ToSlash(relative), info.Mode().String())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, _ = hash.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func TestSetupCmuxAvailableUsesPingNotLookPath(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	setupLookPath = func(name string) (string, error) {
		if name == launch.LauncherCMux {
			return "/usr/bin/cmux", nil
		}
		return "", fs.ErrNotExist
	}
	setupCmuxAvailable = func() bool { return false }
	lookPathOnly, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"--preview", "--json", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(setupPreviewLaunchers(t, lookPathOnly), launch.LauncherCMux) {
		t.Fatalf("LookPath-only cmux was treated as available: %s", lookPathOnly)
	}

	setupLookPath = func(string) (string, error) { return "", fs.ErrNotExist }
	setupCmuxAvailable = func() bool { return true }
	pingOnly, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"--preview", "--json", "--agents", "claude", "--default-session", "collab", "--launcher-preference", "commands"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(setupPreviewLaunchers(t, pingOnly), launch.LauncherCMux) {
		t.Fatalf("ping-available bundle cmux was omitted: %s", pingOnly)
	}
	_ = project
}

func TestSetupDetectGhosttyUsesPingNotLookPath(t *testing.T) {
	_ = setupProjectFixture(t, "claude")
	setupLookPath = func(name string) (string, error) {
		if name == launch.LauncherGhostty {
			return "/usr/bin/ghostty", nil
		}
		return "", fs.ErrNotExist
	}
	setupGhosttyAvailable = func() bool { return false }
	got := detectSetupLaunchers()
	if slices.Contains(got, launch.LauncherGhostty) {
		t.Fatalf("LookPath without ping listed ghostty: %v", got)
	}
	setupGhosttyAvailable = func() bool { return true }
	got = detectSetupLaunchers()
	if !slices.Contains(got, launch.LauncherGhostty) {
		t.Fatalf("ping did not list ghostty: %v", got)
	}
}

func setupPreviewLaunchers(t *testing.T, raw string) []string {
	t.Helper()
	var result setupResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode setup preview: %v\n%s", err, raw)
	}
	return result.Preview.AvailableLaunchers
}

func validateSetupMailbox(base, agent string) error {
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fsq.ValidateExistingMailboxLayout(root, agent)
}
