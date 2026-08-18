//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/launchapi"
)

const launchContractV061BaselineCommit = "46fa8e03599f7e7d56b021f91752048838778bce"

func TestCompatibilityFloorAndEndToEndMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes the real amq binary")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	amqBinary := filepath.Join(binDir, "amq")
	build := exec.Command("go", "build", "-o", amqBinary, "./cmd/amq")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real amq: %v\n%s", err, output)
	}

	t.Run("compatibility floor", func(t *testing.T) {
		if launchapi.Compatibility().ContractSemver != "0.61.1" {
			t.Fatalf("contract floor = %q", launchapi.Compatibility().ContractSemver)
		}
		if _, err := launchapi.Negotiate(launchapi.RequirementV1{
			ContractSemver: ">=0.61.0 <0.62.0", IntentVersion: 1, ResultVersion: 1,
			Features: []string{"prepare_apply_v1", "plan_only_commands_v1"},
		}); err != nil {
			t.Fatalf("negotiate supported floor: %v", err)
		}
		if _, err := launchapi.Negotiate(launchapi.RequirementV1{
			ContractSemver: "<0.61.0", IntentVersion: 1, ResultVersion: 1,
		}); err == nil {
			t.Fatal("negotiation accepted a range below the compatibility floor")
		}
	})

	t.Run("commands serialized prepare apply and durable resume", func(t *testing.T) {
		fixture := newPublicLaunchE2EFixture(t, binDir, launch.LauncherCommands)
		intent := fixture.intent(launchapi.ResumePolicyResume, true)
		intentPath := writeLaunchIntentE2E(t, intent)
		env := fixture.env()

		stdout, stderr, exit := runRealAMQWithExit(t, amqBinary, fixture.project, env,
			"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", "commands")
		if exit != ExitActionRequired {
			t.Fatalf("Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var prepared launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &prepared)
		if prepared.Outcome != launchapi.PrepareOutcomeActionRequired ||
			!containsActionKind(prepared.RequiredActions, launchapi.RequiredActionTrustConfirmation) {
			t.Fatalf("initial Prepare = %#v", prepared)
		}
		if !slices.Equal(prepared.Preview.Roster.Desired, []string{"claude", "operator"}) ||
			len(prepared.Preview.Participants) != 2 || prepared.Preview.Participants[1].Handle != "operator" ||
			prepared.Preview.Participants[1].Runnable {
			t.Fatalf("multi-seat preview = %#v", prepared.Preview)
		}

		request := fixture.request(intent, launch.LauncherCommands)
		applyPath := writeApplyRequestE2E(t, request, prepared)
		stdout, stderr, exit = runRealAMQWithExit(t, amqBinary, fixture.project, env,
			"launch", "--apply", applyPath, "--json")
		if exit != ExitActionRequired {
			t.Fatalf("Apply exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var applied launchapi.ApplyResultV1
		decodeRealLaunchJSON(t, stdout, &applied)
		if applied.Outcome != "action_required" || len(applied.Commands) != 1 ||
			!slices.Equal(applied.Roster.Desired, []string{"claude", "operator"}) {
			t.Fatalf("commands Apply = %#v", applied)
		}
		assertManagedCommandOptions(t, applied.Commands[0].Argv, fixture.injector)

		stdout, stderr, exit = runRealAMQWithExit(t, amqBinary, fixture.project, env,
			"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", "commands")
		if exit != ExitActionRequired {
			t.Fatalf("resume Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var resumePrepared launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &resumePrepared)
		if containsActionKind(resumePrepared.RequiredActions, launchapi.RequiredActionTrustConfirmation) ||
			!containsActionKind(resumePrepared.RequiredActions, launchapi.RequiredActionStaleConversation) ||
			!slices.Equal(resumePrepared.Preview.Roster.Present, []string{"claude", "operator"}) ||
			len(resumePrepared.Observations) != 2 {
			t.Fatalf("durable resume actions = %#v", resumePrepared.RequiredActions)
		}
		resumeApplyPath := writeApplyRequestE2E(t, request, resumePrepared)
		stdout, stderr, exit = runRealAMQWithExit(t, amqBinary, fixture.project, env,
			"launch", "--apply", resumeApplyPath, "--json")
		if exit != ExitActionRequired {
			t.Fatalf("fresh-process resume Apply exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var resumed launchapi.ApplyResultV1
		decodeRealLaunchJSON(t, stdout, &resumed)
		if len(resumed.Commands) != 1 || resumed.Outcome != "action_required" {
			t.Fatalf("resume Apply = %#v", resumed)
		}

		if err := fsq.EnsureAgentDirs(fixture.sessionRoot, "reviewer"); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, exit = runRealAMQWithExit(t, amqBinary, fixture.project, env,
			"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", "commands")
		if exit != ExitActionRequired {
			t.Fatalf("roster-drift Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var drifted launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &drifted)
		if !slices.Contains(drifted.Preview.Roster.Extra, "reviewer") {
			t.Fatalf("roster drift = %#v", drifted.Preview.Roster)
		}
	})

	t.Run("sibling git worktree exact root real message", func(t *testing.T) {
		testSiblingWorktreeExactRoot(t, amqBinary, binDir)
	})

	t.Run("tmux serialized prepare apply and readback", func(t *testing.T) {
		if _, err := exec.LookPath("tmux"); err != nil {
			t.Skip("tmux is not installed")
		}
		fixture := newPublicLaunchE2EFixture(t, binDir, launch.LauncherTMux)
		intent := fixture.intent(launchapi.ResumePolicyFresh, false)
		intent.Participants[0].Execution = &launchapi.ExecutionOptionsV1{Wake: launchapi.WakeOptionsV1{
			Mode: launchapi.WakeDisabled, AuditReason: "hermetic tmux matrix row",
		}}
		intentPath := writeLaunchIntentE2E(t, intent)
		socketDir, err := os.MkdirTemp("/tmp", "amq-public-tmux-e2e-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
		env := append(fixture.env(), "TMUX_TMPDIR="+socketDir)
		t.Cleanup(func() { stopHermeticTmuxServer(t, socketDir, fixture.sessionRoot, "claude", false) })

		stdout, stderr, exit := runRealAMQWithExit(t, amqBinary, fixture.project, env,
			"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", "tmux")
		if exit != ExitActionRequired {
			t.Fatalf("tmux Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var prepared launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &prepared)
		applyPath := writeApplyRequestE2E(t, fixture.request(intent, launch.LauncherTMux), prepared)
		stdout, stderr, exit = runRealAMQWithExit(t, amqBinary, fixture.project, env,
			"launch", "--apply", applyPath, "--json")
		if exit != 0 {
			t.Fatalf("tmux Apply exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var applied launchapi.ApplyResultV1
		decodeRealLaunchJSON(t, stdout, &applied)
		if applied.Outcome != "applied" || len(applied.Commands) != 0 {
			t.Fatalf("tmux Apply = %#v", applied)
		}
		rootIdentity, err := fsq.SnapshotDeliveryRoot(fixture.sessionRoot)
		if err != nil {
			t.Fatal(err)
		}
		root, err := fsq.OpenDeliveryRoot(fixture.sessionRoot, rootIdentity)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		ticket, err := launch.LoadExecutionTicket(root, "claude")
		if err != nil || ticket.Execution == nil || ticket.Execution.WakeMode != string(launchapi.WakeDisabled) ||
			ticket.Execution.AuditReason != "hermetic tmux matrix row" {
			t.Fatalf("tmux execution options = %#v, err=%v", ticket.Execution, err)
		}
		waitForProviderLog(t, fixture.providerLog, "--session-id ")

		stdout, stderr, exit = runRealAMQWithExit(t, amqBinary, fixture.project, env,
			"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", "tmux")
		if exit != 0 {
			t.Fatalf("tmux readback Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var readback launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &readback)
		if readback.Outcome != launchapi.PrepareOutcomeReady || len(readback.Observations) != 2 {
			t.Fatalf("tmux readback = %#v", readback)
		}
	})
}

func TestV061PrepareV0611ApplyCompatibility(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes two real amq binaries")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	currentBinary := filepath.Join(binDir, "amq-v0611")
	buildCurrent := exec.Command("go", "build", "-o", currentBinary, "./cmd/amq")
	buildCurrent.Dir = repoRoot
	if output, err := buildCurrent.CombinedOutput(); err != nil {
		t.Fatalf("build v0.61.1 contract binary: %v\n%s", err, output)
	}
	legacyBinary := buildHistoricalLaunchBinary(t, repoRoot, binDir, launchContractV061BaselineCommit)

	fixture := newPublicLaunchE2EFixture(t, binDir, launch.LauncherCommands)
	intent := launchapi.LaunchIntentV1{
		IntentVersion: launchapi.IntentVersionV1,
		Participants:  []launchapi.ParticipantV1{{Handle: "operator", Runnable: false}},
	}
	intentPath := writeLaunchIntentE2E(t, intent)
	env := fixture.env()
	stdout, stderr, exit := runRealAMQWithExit(t, legacyBinary, fixture.project, env,
		"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", "commands")
	if exit != 0 {
		t.Fatalf("v0.61.0 Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
	}
	var legacyPrepared struct {
		SubjectDigest string `json:"subject_digest"`
	}
	if err := json.Unmarshal(stdout, &legacyPrepared); err != nil {
		t.Fatalf("decode v0.61.0 Prepare: %v\n%s", err, stdout)
	}
	if legacyPrepared.SubjectDigest == "" || bytes.Contains(stdout, []byte(`"subject_schema"`)) {
		t.Fatalf("legacy Prepare shape changed: %s", stdout)
	}
	request := fixture.request(intent, launch.LauncherCommands)
	applyData, err := json.Marshal(struct {
		RequestVersion int                        `json:"request_version"`
		Prepare        launchapi.PrepareRequestV1 `json:"prepare"`
		SubjectDigest  string                     `json:"subject_digest"`
		Decisions      []launchapi.DecisionV1     `json:"decisions"`
	}{
		RequestVersion: launchapi.RequestVersionV1,
		Prepare:        request,
		SubjectDigest:  legacyPrepared.SubjectDigest,
		Decisions:      []launchapi.DecisionV1{},
	})
	if err != nil {
		t.Fatal(err)
	}
	applyPath := filepath.Join(t.TempDir(), "legacy-apply.json")
	if err := os.WriteFile(applyPath, applyData, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exit = runRealAMQWithExit(t, currentBinary, fixture.project, env,
		"launch", "--apply", applyPath, "--json")
	if exit != 0 {
		t.Fatalf("v0.61.1 Apply exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
	}
	var applied launchapi.ApplyResultV1
	decodeRealLaunchJSON(t, stdout, &applied)
	if applied.ReasonCode == "subject_changed" || applied.SubjectSchema != launchapi.SubjectSchemaV1 ||
		!slices.Equal(applied.Hints, []launchapi.ResultHintV1{launchapi.HintReprepareRecommended}) {
		t.Fatalf("cross-version Apply = %#v", applied)
	}

	stdout, stderr, exit = runRealAMQWithExit(t, currentBinary, fixture.project, env,
		"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", "commands")
	if exit != 0 {
		t.Fatalf("v0.61.1 re-Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
	}
	var upgraded launchapi.PrepareResultV1
	decodeRealLaunchJSON(t, stdout, &upgraded)
	if upgraded.SubjectSchema != launchapi.SubjectSchemaV2 || upgraded.SubjectDigest == legacyPrepared.SubjectDigest {
		t.Fatalf("re-Prepare did not upgrade schema: legacy=%s upgraded=%#v", legacyPrepared.SubjectDigest, upgraded)
	}
}

func buildHistoricalLaunchBinary(t *testing.T, repoRoot, binDir, commit string) string {
	t.Helper()
	archivePath := filepath.Join(t.TempDir(), "source.tar")
	archive := exec.Command("git", "archive", "--format=tar", "--output", archivePath, commit)
	archive.Dir = repoRoot
	if output, err := archive.CombinedOutput(); err != nil {
		t.Skipf("v0.61.0 contract commit %s unavailable: %v\n%s", commit, err, output)
	}
	sourceDir := t.TempDir()
	extract := exec.Command("tar", "-xf", archivePath, "-C", sourceDir)
	if output, err := extract.CombinedOutput(); err != nil {
		t.Fatalf("extract v0.61.0 contract source: %v\n%s", err, output)
	}
	binary := filepath.Join(binDir, "amq-v0610")
	build := exec.Command("go", "build", "-o", binary, "./cmd/amq")
	build.Dir = sourceDir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build v0.61.0 contract binary: %v\n%s", err, output)
	}
	return binary
}

type publicLaunchE2EFixture struct {
	project     string
	sessionRoot string
	home        string
	state       string
	binDir      string
	provider    string
	providerLog string
	injector    string
}

func newPublicLaunchE2EFixture(t *testing.T, binDir, launcher string) publicLaunchE2EFixture {
	t.Helper()
	project := t.TempDir()
	canonicalProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	providerLog := filepath.Join(t.TempDir(), "provider.log")
	provider := filepath.Join(t.TempDir(), "claude")
	providerScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  --version) echo "1.0.0 (Claude Code)"; exit 0 ;;
  --help) echo "--session-id <uuid> --resume [value]"; exit 0 ;;
esac
printf '%%s\n' "$*" >> %s
exec /bin/sleep 60
`, shellQuoteArg(providerLog))
	if err := os.WriteFile(provider, []byte(providerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	injector := filepath.Join(t.TempDir(), "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writePublicLaunchProjectConfig(t, canonicalProject, launcher)
	sessionRoot := filepath.Join(canonicalProject, defaultCoopRoot, "collab")
	for _, root := range []string{filepath.Dir(sessionRoot), sessionRoot} {
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(`{"version":1,"agents":["claude"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsq.EnsureAgentDirs(sessionRoot, "claude"); err != nil {
		t.Fatal(err)
	}
	return publicLaunchE2EFixture{
		project: canonicalProject, sessionRoot: sessionRoot, home: t.TempDir(), state: t.TempDir(),
		binDir: binDir, provider: provider, providerLog: providerLog, injector: injector,
	}
}

func (f publicLaunchE2EFixture) intent(policy launchapi.ResumePolicy, symphony bool) launchapi.LaunchIntentV1 {
	execution := &launchapi.ExecutionOptionsV1{
		RequireWake: true, NoGitignore: true,
		Wake: launchapi.WakeOptionsV1{
			Mode:     launchapi.WakeEnabled,
			Injector: &launchapi.InjectorOptionsV1{Mode: launchapi.InjectorRaw, Via: f.injector, Args: []string{"exec", "terminal-a"}},
		},
	}
	if symphony {
		execution.Integrations.Symphony = &launchapi.SymphonyOptionsV1{
			Events: []launchapi.SymphonyEvent{
				launchapi.SymphonyAfterCreate, launchapi.SymphonyBeforeRun,
				launchapi.SymphonyAfterRun, launchapi.SymphonyBeforeRemove,
			},
			WorkspaceKey: "workspace-7",
		}
	}
	return launchapi.LaunchIntentV1{
		IntentVersion: launchapi.IntentVersionV1,
		Participants: []launchapi.ParticipantV1{
			{
				Handle: "claude", Runnable: true, Executable: f.provider,
				Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: f.project},
				ResumePolicy: policy, Execution: execution,
			},
			{Handle: "operator", Runnable: false},
		},
	}
}

func (f publicLaunchE2EFixture) request(intent launchapi.LaunchIntentV1, launcher string) launchapi.PrepareRequestV1 {
	return launchapi.PrepareRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Target:         launchapi.TargetV1{ProjectRoot: f.project, SessionRoot: f.sessionRoot, Session: "collab"},
		Launcher:       launcher, Intent: intent,
	}
}

func (f publicLaunchE2EFixture) env() []string {
	return append(cleanLaunchE2EEnv(),
		"HOME="+f.home, "XDG_STATE_HOME="+f.state,
		"PATH="+f.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
}

func writePublicLaunchProjectConfig(t *testing.T, project, launcher string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectData, err := launch.MarshalProjectConfig(launch.ProjectConfig{
		Schema: launch.ProjectConfigSchema, DefaultSession: "collab", Layout: launch.LayoutIntent{Type: launch.LayoutColumns},
		Agents: []launch.ProjectAgentConfig{{Handle: "claude", Adapter: launch.ClaudeProvider, Command: []string{launch.ClaudeProvider}, ResumePolicy: launch.ResumeEnabled}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, setupConfigPath), projectData, 0o600); err != nil {
		t.Fatal(err)
	}
	preference := []string{launcher}
	if launcher != launch.LauncherCommands {
		preference = append(preference, launch.LauncherCommands)
	}
	localData, err := launch.MarshalLocalConfig(launch.LocalConfig{
		Schema: launch.LocalConfigSchema, LauncherPreference: preference,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, setupLocalConfigPath), localData, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLaunchIntentE2E(t *testing.T, intent launchapi.LaunchIntentV1) string {
	t.Helper()
	data, err := launchapi.MarshalLaunchIntentV1(intent)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "intent.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeApplyRequestE2E(t *testing.T, request launchapi.PrepareRequestV1, prepared launchapi.PrepareResultV1) string {
	t.Helper()
	decisions := make([]launchapi.DecisionV1, 0, len(prepared.RequiredActions))
	for _, action := range prepared.RequiredActions {
		choice := action.AllowedDecisions[0]
		switch action.Kind {
		case launchapi.RequiredActionTrustConfirmation:
			choice = launchapi.DecisionTrustExactSubject
		case launchapi.RequiredActionStaleConversation:
			choice = launchapi.DecisionFreshOnce
		case launchapi.RequiredActionRebindConfirmation:
			choice = launchapi.DecisionCloseOld
		case launchapi.RequiredActionUnsupportedCapability:
			choice = launchapi.DecisionAcceptDegraded
		}
		if !slices.Contains(action.AllowedDecisions, choice) {
			t.Fatalf("action %s does not allow %s: %#v", action.ActionID, choice, action.AllowedDecisions)
		}
		decisions = append(decisions, launchapi.DecisionV1{ActionID: action.ActionID, Choice: choice})
	}
	data, err := json.Marshal(launchapi.ApplyRequestV1{
		RequestVersion: launchapi.RequestVersionV1, Prepare: request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: decisions,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "apply.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runRealAMQWithExit(t *testing.T, binary, project string, env []string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = project, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("real amq %v timed out: %v\n%s", args, ctx.Err(), stderr.Bytes())
	}
	if err == nil {
		return stdout.Bytes(), stderr.Bytes(), 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("real amq %v failed to execute: %v\n%s", args, err, stderr.Bytes())
	}
	return stdout.Bytes(), stderr.Bytes(), exitError.ExitCode()
}

func cleanLaunchE2EEnv() []string {
	blocked := []string{
		"AM_ROOT", "AM_BASE_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT_ID", "AM_SESSION", "AMQ_GLOBAL_ROOT", "AM_ME",
		"HOME", "XDG_STATE_HOME", "PATH", "TMUX_TMPDIR",
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(blocked, key) {
			env = append(env, entry)
		}
	}
	return env
}

func decodeRealLaunchJSON(t *testing.T, data []byte, result any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		t.Fatalf("decode real launch output: %v\n%s", err, data)
	}
}

func containsActionKind(actions []launchapi.RequiredActionV1, kind launchapi.RequiredActionKindV1) bool {
	return slices.ContainsFunc(actions, func(action launchapi.RequiredActionV1) bool { return action.Kind == kind })
}

func assertManagedCommandOptions(t *testing.T, argv []string, injector string) {
	t.Helper()
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{
		"--require-wake", "--no-gitignore", "--wake-inject-mode\x00raw",
		"--wake-inject-via\x00" + injector, "--wake-inject-arg\x00exec", "--wake-inject-arg\x00terminal-a",
		"--managed-symphony-workspace-key\x00workspace-7",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("managed command omitted %q: %#v", want, argv)
		}
	}
	if strings.Count(joined, "--managed-symphony-event") != 4 {
		t.Fatalf("managed command events = %#v", argv)
	}
}

func testSiblingWorktreeExactRoot(t *testing.T, amqBinary, binDir string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	runGit(repo, "init", "-b", "main")
	runGit(repo, "config", "user.email", "test@example.invalid")
	runGit(repo, "config", "user.name", "AMQ Test")
	writePublicLaunchProjectConfig(t, repo, launch.LauncherCommands)
	runGit(repo, "add", ".amq", ".amqrc")
	runGit(repo, "commit", "-m", "fixture")
	sibling := filepath.Join(filepath.Dir(repo), "sibling")
	runGit(repo, "worktree", "add", "-b", "sibling", sibling)

	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSibling, err := filepath.EvalSymlinks(sibling)
	if err != nil {
		t.Fatal(err)
	}
	sessionRoot := filepath.Join(canonicalRepo, defaultCoopRoot, "collab")
	for _, root := range []string{filepath.Dir(sessionRoot), sessionRoot} {
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(`{"version":1,"agents":["claude","codex"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, handle := range []string{"claude", "codex"} {
		if err := fsq.EnsureAgentDirs(sessionRoot, handle); err != nil {
			t.Fatal(err)
		}
	}

	providerDir := t.TempDir()
	claudeProvider := filepath.Join(providerDir, "claude")
	codexProvider := filepath.Join(providerDir, "codex")
	wrapperPath := filepath.Join(providerDir, "seat-wrapper")
	wrapperRecord := filepath.Join(providerDir, "wrapper-argv")
	proofScript := func(capabilities string) string {
		return `#!/bin/sh
set -eu
if [ -n "${AM_ME:-}" ]; then
  case "$AM_ME" in
    claude) exec amq --no-update-check send --root "$AM_ROOT" --me claude --to codex --body "exact-root-message" ;;
    codex) exec amq --no-update-check drain --root "$AM_ROOT" --me codex --include-body ;;
    *) exit 64 ;;
  esac
fi
` + capabilities
	}
	if err := os.WriteFile(claudeProvider, []byte(proofScript(`case "${1:-}" in
  --version) echo "1.0.0 (Claude Code)" ;;
  --help) echo "--session-id <uuid> --resume [value]" ;;
  *) exit 64 ;;
esac
`)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexProvider, []byte(proofScript(`case "${1:-}" in
  --version) echo "codex-cli 0.147.0" ;;
  --help) echo "commands: resume" ;;
  resume) [ "${2:-}" = "--help" ] && echo "Usage: codex resume [OPTIONS] [SESSION_ID]" ;;
  app-server) [ "${2:-}" = "--help" ] && echo "generate-json-schema" ;;
  *) exit 64 ;;
esac
`)), 0o700); err != nil {
		t.Fatal(err)
	}
	wrapperScript := `#!/bin/sh
set -eu
printf '%s\n' "$@" > ` + shellQuoteArg(wrapperRecord) + `
[ "${1:-}" = "--profile" ]
[ "${2:-}" = "lead" ]
shift 2
exec "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(wrapperScript), 0o700); err != nil {
		t.Fatal(err)
	}

	disabledWake := func() *launchapi.ExecutionOptionsV1 {
		return &launchapi.ExecutionOptionsV1{Wake: launchapi.WakeOptionsV1{
			Mode: launchapi.WakeDisabled, AuditReason: "hermetic exact-root proof",
		}}
	}
	intent := launchapi.LaunchIntentV1{IntentVersion: launchapi.IntentVersionV1, Participants: []launchapi.ParticipantV1{
		{
			Handle: "claude", Runnable: true, Executable: claudeProvider,
			Args:         []string{"--allowedTools", "Read"},
			Wrapper:      &launchapi.WrapperV1{Executable: wrapperPath, Args: []string{"--profile", "lead"}},
			Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: canonicalRepo},
			ResumePolicy: launchapi.ResumePolicyFresh, Execution: disabledWake(),
		},
		{
			Handle: "codex", Runnable: true, Executable: codexProvider,
			Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: canonicalSibling},
			ResumePolicy: launchapi.ResumePolicyFresh, Execution: disabledWake(),
		},
	}}
	intentPath := writeLaunchIntentE2E(t, intent)
	env := append(cleanLaunchE2EEnv(), "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdout, stderr, exit := runRealAMQWithExit(t, amqBinary, canonicalRepo, env,
		"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", "commands")
	if exit != ExitActionRequired {
		t.Fatalf("exact-root Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
	}
	var prepared launchapi.PrepareResultV1
	decodeRealLaunchJSON(t, stdout, &prepared)
	request := launchapi.PrepareRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Target:         launchapi.TargetV1{ProjectRoot: canonicalRepo, SessionRoot: sessionRoot, Session: "collab"},
		Launcher:       launch.LauncherCommands, Intent: intent,
	}
	applyPath := writeApplyRequestE2E(t, request, prepared)
	stdout, stderr, exit = runRealAMQWithExit(t, amqBinary, canonicalRepo, env,
		"launch", "--apply", applyPath, "--json")
	if exit != ExitActionRequired {
		t.Fatalf("exact-root Apply exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
	}
	var applied launchapi.ApplyResultV1
	decodeRealLaunchJSON(t, stdout, &applied)
	if len(applied.Commands) != 2 {
		t.Fatalf("exact-root Apply = %#v", applied)
	}
	commands := make(map[string]launchapi.CommandV1, len(applied.Commands))
	for _, command := range applied.Commands {
		handle := commandArgValue(command.Argv, "--me")
		if commandArgValue(command.Argv, "--root") != sessionRoot || slices.Contains(command.Argv, "--session") {
			t.Fatalf("%s command does not use one exact root: %#v", handle, command.Argv)
		}
		commands[handle] = command
	}
	for _, handle := range []string{"claude", "codex"} {
		command, ok := commands[handle]
		if !ok {
			t.Fatalf("missing emitted command for %s: %#v", handle, applied.Commands)
		}
		cmd := exec.Command(command.Argv[0], command.Argv[1:]...)
		cmd.Dir = command.Cwd
		cmd.Env = append(cleanLaunchE2EEnv(), "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "AMQ_NO_UPDATE_CHECK=1")
		for key, value := range command.EnvOverlay {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run %s emitted command: %v\n%s", handle, err, output)
		}
		if handle == "codex" && !strings.Contains(string(output), "exact-root-message") {
			t.Fatalf("sibling command did not drain the real message:\n%s", output)
		}
	}
	recorded, err := os.ReadFile(wrapperRecord)
	if err != nil {
		t.Fatal(err)
	}
	claudeNonce := commands["claude"].EnvOverlay[launch.InternalLaunchNonceEnv]
	if claudeNonce == "" {
		t.Fatal("emitted Claude command omitted managed launch nonce")
	}
	wantWrapperArgv := []string{"--profile", "lead", claudeProvider, "--allowedTools", "Read", "--session-id", claudeNonce}
	gotWrapperArgv := strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n")
	if !slices.Equal(gotWrapperArgv, wantWrapperArgv) {
		t.Fatalf("real wrapper argv = %#v, want %#v", gotWrapperArgv, wantWrapperArgv)
	}
	if _, err := os.Stat(filepath.Join(canonicalSibling, defaultCoopRoot)); !os.IsNotExist(err) {
		t.Fatalf("sibling-local queue exists under %s: %v", canonicalSibling, err)
	}
}

func commandArgValue(argv []string, name string) string {
	for index := 0; index+1 < len(argv); index++ {
		if argv[index] == name {
			return argv[index+1]
		}
	}
	return ""
}
