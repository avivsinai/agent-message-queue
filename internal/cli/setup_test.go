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
