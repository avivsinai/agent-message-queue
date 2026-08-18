//go:build !windows

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/launchapi"
)

// Adoption-smoke oracle for Omri's seven #480/#557 findings (addendum
// /private/tmp/amq-480-report/wb3-addendum-draft.md). Each named row asserts
// the current outcome so CI stays green. Each WB3 bead flips its rows to the
// addendum expected outcome; that flip is the visible acceptance.
//
//	Row | Finding | Today | WB3 expected
//	finding1_missing_profile_base_root | 1 named-profile base root | missing .agent-mail/<profile> refuses (not found / stat delivery root) | Prepare planned write create_base_root; Apply creates the authorized base
//	finding2_mixed_live_fresh_keep_creates | 2 live-seat dispositions | on_live keep -> kept + created missing seats | omitted on_live still refuses
//	finding3_wrapper_unknown_field | 3 wrapper | strict decode unknown field "wrapper" | wrapper argv prepended to full provider argv
//	finding4_placement_unsupported | 4 placement | AMQ_SQUAD_TMUX_* env rejected as provider env | placement preview; unsupported tuple -> placement_unsupported
//	finding5_positional_bootstrap_prompt / finding5_claude_allowed_tools / finding5_codex_dash_c | 5 materialized argv | raw positional prompt rejected; exact --allowedTools and -c forms admitted | initial_input + --allowedTools / -c admitted
//	finding6_caller_context_bound_and_echoed | 6 caller correlation | caller_context is subject-bound and echoed on Apply/evidence/lifecycle
//	finding7_same_path_inode_replacement_subject_changed | 7 physical identity | inode replacement -> subject_changed; plan/trust unchanged | (landed)
//
// Real bootstrap text from the squad-v2-29-4 compiler, not the placeholder in
// #480 comment 5301600497. Finding 5's positional prompt is this string.
const omriSquadBootstrapPrompt = `You are the lead seat on squad-v2-29-4 session v2-29-6.
Coordinate with senior-dev and fullstack over AMQ in this repo.
Drain your inbox, send kind=status when work starts, and keep the existing p2p thread.
Do not relaunch teammates that are already live.`

func TestAdoptionSmokeOmriSquadV2294(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes the real amq binary")
	}
	amqBinary := buildAdoptionSmokeAMQ(t)

	t.Run("finding5_positional_bootstrap_prompt", func(t *testing.T) {
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
		intent := fx.legalSquadIntent()
		intent.Participants[1].Args = append(intent.Participants[1].Args, omriSquadBootstrapPrompt)
		stdout, stderr, exit := fx.prepare(t, intent, launch.LauncherCommands)
		assertAdoptionDecodeRejection(t, exit, stdout, stderr, "You are the lead seat on squad-v2-29-4")
	})

	t.Run("finding5_claude_allowed_tools", func(t *testing.T) {
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
		intent := fx.legalSquadIntent()
		intent.Participants[1].Args = append(intent.Participants[1].Args, "--allowedTools", "Bash,Read,Write,Edit")
		stdout, stderr, exit := fx.prepare(t, intent, launch.LauncherCommands)
		assertAdoptionArgumentAdmitted(t, exit, stdout, stderr, "lead", "--allowedTools", "Bash,Read,Write,Edit")
	})

	t.Run("finding5_codex_dash_c", func(t *testing.T) {
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
		intent := fx.legalSquadIntent()
		intent.Participants[2].Args = append(intent.Participants[2].Args, "-c", "model_reasoning_effort=high")
		stdout, stderr, exit := fx.prepare(t, intent, launch.LauncherCommands)
		assertAdoptionArgumentAdmitted(t, exit, stdout, stderr, "senior-dev", "-c", "model_reasoning_effort=high")
	})

	t.Run("finding4_placement_unsupported", func(t *testing.T) {
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
		intentPath := filepath.Join(t.TempDir(), "intent.json")
		if err := os.WriteFile(intentPath, fx.intentJSON(t, fx.legalSquadIntent()), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, exit := runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env,
			"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", launch.LauncherCommands,
			"--session", fx.session, "--placement", `{"target":"current_window","layout":"columns","launcher_pane":"%323"}`)
		if exit != ExitActionRequired {
			t.Fatalf("Prepare unsupported placement exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		var prepared launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &prepared)
		if prepared.Outcome != launchapi.PrepareOutcomeUnsupported || prepared.Reason != launch.PlacementUnsupportedReason {
			t.Fatalf("Prepare placement = %#v stderr=%s", prepared, stderr)
		}
		if prepared.Preview.Placement == nil || prepared.Preview.Placement.Supported ||
			prepared.Preview.Placement.ReasonCode != launch.PlacementUnsupportedReason ||
			prepared.Preview.Placement.Requested == nil || prepared.Preview.Placement.Requested.LauncherPane != "%323" {
			t.Fatalf("placement preview = %#v", prepared.Preview.Placement)
		}
		if _, err := os.Stat(launch.BindingPath(fx.sessionRoot)); !os.IsNotExist(err) {
			t.Fatalf("unsupported placement mutated binding: %v", err)
		}
	})

	t.Run("finding3_wrapper_unknown_field", func(t *testing.T) {
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
		raw := fx.intentJSON(t, fx.legalSquadIntent())
		raw = injectParticipantJSONField(t, raw, 1, "wrapper", map[string]any{
			"executable": "/opt/company/bin/seat-wrapper",
			"args":       []string{"--profile", "lead"},
		})
		stdout, stderr, exit := fx.prepareJSON(t, raw, launch.LauncherCommands)
		assertAdoptionUnknownField(t, exit, stdout, stderr, "wrapper")
	})

	t.Run("finding6_caller_context_bound_and_echoed", func(t *testing.T) {
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
		raw := fx.applyEnvelopeJSON(t, fx.legalSquadIntent(), launch.LauncherCommands, "sha256:"+strings.Repeat("a", 64))
		raw = injectPrepareJSONField(t, raw, "caller_context", map[string]any{"run_id": "v2-29-6"})
		raw = []byte(strings.Replace(string(raw), `"subject_digest":`, `"subject_schema":2,"subject_digest":`, 1))
		stdout, stderr, exit := fx.applyJSONBytes(t, raw)
		if exit != ExitActionRequired {
			t.Fatalf("caller context Apply exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var result launchapi.ApplyResultV1
		decodeRealLaunchJSON(t, stdout, &result)
		if result.ReasonCode != "subject_changed" || len(result.CallerContext) != 1 || result.CallerContext["run_id"] != "v2-29-6" {
			t.Fatalf("caller context Apply = %#v", result)
		}
	})

	t.Run("finding1_missing_profile_base_root", func(t *testing.T) {
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "v2-29-6")
		intent := launchapi.LaunchIntentV1{
			IntentVersion: launchapi.IntentVersionV1,
			Participants:  []launchapi.ParticipantV1{{Handle: "user", Runnable: false}},
		}
		baseRoot := filepath.Join(fx.project, ".agent-mail", "squad-v2-29-4")
		sessionRoot := filepath.Join(baseRoot, fx.session)
		request := launchapi.PrepareRequestV1{
			RequestVersion: launchapi.RequestVersionV1,
			Target: launchapi.TargetV1{
				ProjectRoot: fx.project, BaseRoot: baseRoot, SessionRoot: sessionRoot, Session: fx.session,
			},
			Launcher: launch.LauncherCommands, Intent: intent,
		}
		prepared, err := launchapi.Prepare(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if len(prepared.PlannedWrites) != 1 || prepared.PlannedWrites[0].Kind != launchapi.PlannedWriteCreateBaseRoot || prepared.PlannedWrites[0].Path != baseRoot {
			t.Fatalf("base-root planned writes = %#v", prepared.PlannedWrites)
		}
		if _, err := os.Lstat(baseRoot); !os.IsNotExist(err) {
			t.Fatalf("Prepare created base root: %v", err)
		}

		applyPath := writeApplyRequestE2E(t, request, prepared)
		stdout, stderr, exit := runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env,
			"launch", "--apply", applyPath, "--json")
		if exit != 0 {
			t.Fatalf("base-root Apply exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		var applied launchapi.ApplyResultV1
		decodeRealLaunchJSON(t, stdout, &applied)
		if applied.Outcome != "provisioned_no_runnable" {
			t.Fatalf("base-root Apply = %#v", applied)
		}
		for _, path := range []string{baseRoot, sessionRoot} {
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("created %s: info=%v err=%v", path, info, err)
			}
		}
	})

	t.Run("finding2_mixed_live_fresh_keep_creates", func(t *testing.T) {
		if _, err := exec.LookPath("tmux"); err != nil {
			t.Skip("tmux is not installed")
		}
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
		socketDir, err := os.MkdirTemp("/tmp", "amq-09i-tmux-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
		fx.env = append(fx.env, "TMUX_TMPDIR="+socketDir)
		t.Cleanup(func() { stopHermeticTmuxServer(t, socketDir, fx.sessionRoot, "lead", false) })

		liveIntent := fx.legalSquadIntent()
		liveIntent.Participants = liveIntent.Participants[:2] // user + lead
		liveIntent.Participants[1].ResumePolicy = launchapi.ResumePolicyFresh
		liveIntent.Participants[1].Execution = disabledWakeExecution()
		stdout, stderr, exit := fx.prepare(t, liveIntent, launch.LauncherTMux)
		if exit != ExitActionRequired {
			t.Fatalf("live Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var prepared launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &prepared)
		applyPath := writeApplyRequestE2E(t, fx.request(liveIntent, launch.LauncherTMux), prepared)
		stdout, stderr, exit = runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env, "launch", "--apply", applyPath, "--json")
		if exit != 0 {
			t.Fatalf("live Apply exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		waitForProviderLog(t, fx.claudeLog, "--session-id ")
		before := fx.liveSeatSnapshot(t, socketDir)

		mixed := fx.legalSquadIntent()
		mixed.Participants[1].ResumePolicy = launchapi.ResumePolicyResume
		mixed.Participants[1].Execution = disabledWakeExecution()
		mixed.Participants[1].OnLive = launchapi.OnLiveKeep
		// Create fullstack (Claude mint). Hermetic Codex never emits notify, so
		// Apply cannot commit senior-dev in-process.
		mixed.Participants[3].ResumePolicy = launchapi.ResumePolicyFresh
		mixed.Participants[3].Execution = disabledWakeExecution()
		mixed.Participants = []launchapi.ParticipantV1{
			mixed.Participants[0], mixed.Participants[1], mixed.Participants[3],
		}
		stdout, stderr, exit = fx.prepare(t, mixed, launch.LauncherTMux)
		if exit != 0 && exit != ExitActionRequired {
			t.Fatalf("mixed keep Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var mixedPrepared launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &mixedPrepared)
		applyPath = writeApplyRequestE2E(t, fx.request(mixed, launch.LauncherTMux), mixedPrepared)
		stdout, stderr, exit = runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env, "launch", "--apply", applyPath, "--json")
		if exit != 0 {
			t.Fatalf("mixed keep Apply exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var applied launchapi.ApplyResultV1
		decodeRealLaunchJSON(t, stdout, &applied)
		after := fx.liveSeatSnapshot(t, socketDir)
		if after.LeadConversation != before.LeadConversation {
			t.Fatalf("keep mutated lead conversation\nbefore=%s\nafter=%s", before.LeadConversation, after.LeadConversation)
		}
		_, createdErr := os.Stat(launch.ConversationPath(fx.sessionRoot, "fullstack"))
		if createdErr != nil || after.Panes != 2 || after.SessionIDs != 1 {
			t.Fatalf("keep+create snapshot = %+v created=%v", after, createdErr)
		}
		kept, created := 0, 0
		for _, observation := range applied.Observations {
			switch observation.Disposition {
			case launch.SeatKept:
				kept++
			case launch.SeatCreated:
				created++
			}
		}
		if kept != 1 || created != 1 {
			t.Fatalf("dispositions kept=%d created=%d observations=%#v", kept, created, applied.Observations)
		}
	})

	t.Run("finding7_same_path_inode_replacement_subject_changed", func(t *testing.T) {
		fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
		intent := fx.legalSquadIntent()
		intent.Participants = intent.Participants[:2]
		intent.Participants[1].Execution = disabledWakeExecution()
		stdout, stderr, exit := fx.prepare(t, intent, launch.LauncherCommands)
		if exit != ExitActionRequired {
			t.Fatalf("first Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var first launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &first)
		firstApplyPath := writeApplyRequestE2E(t, fx.request(intent, launch.LauncherCommands), first)

		replacement := fx.claudePath + ".next"
		if err := os.WriteFile(replacement, []byte(fmtAdoptionProvider("1.0.0 (Claude Code)", "--session-id <uuid> --resume [value]", fx.claudeLog)+"# replaced inode\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, fx.claudePath); err != nil {
			t.Fatal(err)
		}

		stdout, stderr, exit = fx.prepare(t, intent, launch.LauncherCommands)
		if exit != ExitActionRequired {
			t.Fatalf("second Prepare exit=%d stderr=%s stdout=%s", exit, stderr, stdout)
		}
		var second launchapi.PrepareResultV1
		decodeRealLaunchJSON(t, stdout, &second)
		if first.SubjectDigest == second.SubjectDigest {
			t.Fatalf("inode replacement kept subject digest %s", first.SubjectDigest)
		}
		if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest {
			t.Fatalf("inode replacement churned plan/trust first=%s/%s second=%s/%s",
				first.PlanDigest, first.TrustDigest, second.PlanDigest, second.TrustDigest)
		}

		stdout, stderr, exit = runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env, "launch", "--apply", firstApplyPath, "--json")
		if len(stdout) == 0 {
			t.Fatalf("stale Apply of first subject produced no JSON: exit=%d stderr=%s", exit, stderr)
		}
		var applied launchapi.ApplyResultV1
		decodeRealLaunchJSON(t, stdout, &applied)
		if applied.ReasonCode != "subject_changed" {
			t.Fatalf("inode replacement Apply reason=%q want subject_changed stdout=%s stderr=%s", applied.ReasonCode, stdout, stderr)
		}
	})
}

func TestAdoptionSmokeLiveLocalBinary(t *testing.T) {
	if os.Getenv("AMQ_LAUNCH_LIVE") != "1" {
		t.Skip("AMQ_LAUNCH_LIVE=1 required; uses a locally built amq with tmux on PATH")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("AMQ_LAUNCH_LIVE=1 requires tmux on PATH: %v", err)
	}
	amqBinary := buildAdoptionSmokeAMQ(t)
	fx := newAdoptionSmokeFixture(t, amqBinary, ".agent-mail", "collab")
	intent := fx.legalSquadIntent()
	intent.Participants[1].Args = append(intent.Participants[1].Args, omriSquadBootstrapPrompt)
	stdout, stderr, exit := fx.prepare(t, intent, launch.LauncherCommands)
	assertAdoptionDecodeRejection(t, exit, stdout, stderr, "You are the lead seat on squad-v2-29-4")
}

type adoptionSmokeFixture struct {
	project     string
	sessionRoot string
	session     string
	amqBinary   string
	claudePath  string
	codexPath   string
	claudeLog   string
	fullstack   string
	env         []string
}

func buildAdoptionSmokeAMQ(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	amqBinary := filepath.Join(t.TempDir(), "amq")
	build := exec.Command("go", "build", "-o", amqBinary, "./cmd/amq")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real amq: %v\n%s", err, output)
	}
	return amqBinary
}

func newAdoptionSmokeFixture(t *testing.T, amqBinary, mailRoot, session string) adoptionSmokeFixture {
	t.Helper()
	project := t.TempDir()
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	claudeLog := filepath.Join(t.TempDir(), "claude.log")
	claudePath := filepath.Join(binDir, "claude")
	codexPath := filepath.Join(binDir, "codex")
	if err := os.WriteFile(claudePath, []byte(fmtAdoptionProvider("1.0.0 (Claude Code)", "--session-id <uuid> --resume [value]", claudeLog)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte(`#!/bin/sh
case "$1" in
  --version) echo "codex-cli 0.147.0"; exit 0 ;;
  --help) echo "commands: resume"; exit 0 ;;
  resume) [ "${2:-}" = "--help" ] && echo "Usage: codex resume [OPTIONS] [SESSION_ID]"; exit 0 ;;
  app-server) [ "${2:-}" = "--help" ] && echo "generate-json-schema"; exit 0 ;;
esac
exit 0
`), 0o700); err != nil {
		t.Fatal(err)
	}
	fullstack := filepath.Join(filepath.Dir(canonical), filepath.Base(canonical)+"-fullstack")
	if err := os.Mkdir(fullstack, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fullstack) })

	if err := os.WriteFile(filepath.Join(canonical, ".amqrc"), []byte(`{"root":"`+mailRoot+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(canonical, ".amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectData, err := launch.MarshalProjectConfig(launch.ProjectConfig{
		Schema: launch.ProjectConfigSchema, DefaultSession: session, Layout: launch.LayoutIntent{Type: launch.LayoutColumns},
		Agents: []launch.ProjectAgentConfig{
			{Handle: "lead", Adapter: launch.ClaudeProvider, Command: []string{launch.ClaudeProvider}, ResumePolicy: launch.ResumeEnabled},
			{Handle: "senior-dev", Adapter: launch.CodexProvider, Command: []string{launch.CodexProvider}, ResumePolicy: launch.ResumeEnabled},
			{Handle: "fullstack", Adapter: launch.ClaudeProvider, Command: []string{launch.ClaudeProvider}, ResumePolicy: launch.ResumeEnabled},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, setupConfigPath), projectData, 0o600); err != nil {
		t.Fatal(err)
	}
	localData, err := launch.MarshalLocalConfig(launch.LocalConfig{
		Schema: launch.LocalConfigSchema, LauncherPreference: []string{launch.LauncherTMux, launch.LauncherCommands},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, setupLocalConfigPath), localData, 0o600); err != nil {
		t.Fatal(err)
	}

	sessionRoot := filepath.Join(canonical, filepath.FromSlash(mailRoot), session)
	if mailRoot == ".agent-mail" {
		for _, root := range []string{filepath.Dir(sessionRoot), sessionRoot} {
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(`{"version":1,"agents":["user","lead","senior-dev","fullstack"]}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, handle := range []string{"user", "lead", "senior-dev", "fullstack"} {
			if err := fsq.EnsureAgentDirs(sessionRoot, handle); err != nil {
				t.Fatal(err)
			}
		}
	} else if parent := filepath.Dir(filepath.Join(canonical, filepath.FromSlash(mailRoot))); parent != canonical {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	home, state := t.TempDir(), t.TempDir()
	return adoptionSmokeFixture{
		project: canonical, sessionRoot: sessionRoot, session: session, amqBinary: amqBinary,
		claudePath: claudePath, codexPath: codexPath, claudeLog: claudeLog, fullstack: fullstack,
		env: append(cleanLaunchE2EEnv(),
			"HOME="+home, "XDG_STATE_HOME="+state, "AMQ_NO_UPDATE_CHECK=1",
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		),
	}
}

func fmtAdoptionProvider(version, help, logPath string) string {
	return `#!/bin/sh
case "$1" in
  --version) echo "` + version + `"; exit 0 ;;
  --help) echo "` + help + `"; exit 0 ;;
esac
printf '%s\n' "$*" >> ` + shellQuoteArg(logPath) + `
exec /bin/sleep 60
`
}

func disabledWakeExecution() *launchapi.ExecutionOptionsV1 {
	return &launchapi.ExecutionOptionsV1{Wake: launchapi.WakeOptionsV1{
		Mode: launchapi.WakeDisabled, AuditReason: "adoption-smoke hermetic",
	}}
}

func rawWakeExecution() *launchapi.ExecutionOptionsV1 {
	return &launchapi.ExecutionOptionsV1{
		RequireWake: true,
		Wake: launchapi.WakeOptionsV1{
			Mode:     launchapi.WakeEnabled,
			Injector: &launchapi.InjectorOptionsV1{Mode: launchapi.InjectorRaw},
		},
	}
}

func (fx adoptionSmokeFixture) legalSquadIntent() launchapi.LaunchIntentV1 {
	return launchapi.LaunchIntentV1{
		IntentVersion: launchapi.IntentVersionV1,
		Participants: []launchapi.ParticipantV1{
			{Handle: "user", Runnable: false},
			{
				Handle: "lead", Runnable: true, Executable: fx.claudePath,
				Args:         []string{"--effort", "high", "--model", "fable"},
				Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: fx.project},
				ResumePolicy: launchapi.ResumePolicyResume, Execution: rawWakeExecution(),
			},
			{
				Handle: "senior-dev", Runnable: true, Executable: fx.codexPath,
				Args:         []string{"--dangerously-bypass-approvals-and-sandbox"},
				Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: fx.project},
				ResumePolicy: launchapi.ResumePolicyResume, Execution: rawWakeExecution(),
			},
			{
				Handle: "fullstack", Runnable: true, Executable: fx.claudePath,
				Args:         []string{"--effort", "high", "--model", "fable"},
				Cwd:          &launchapi.WorkingDirectoryV1{Kind: launchapi.WorkingDirectoryAbsolute, Path: fx.fullstack},
				ResumePolicy: launchapi.ResumePolicyResume, Execution: rawWakeExecution(),
			},
		},
	}
}

func (fx adoptionSmokeFixture) request(intent launchapi.LaunchIntentV1, launcher string) launchapi.PrepareRequestV1 {
	return launchapi.PrepareRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Target:         launchapi.TargetV1{ProjectRoot: fx.project, SessionRoot: fx.sessionRoot, Session: fx.session},
		Launcher:       launcher, Intent: intent,
	}
}

func (fx adoptionSmokeFixture) intentJSON(t *testing.T, intent launchapi.LaunchIntentV1) []byte {
	t.Helper()
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func (fx adoptionSmokeFixture) prepare(t *testing.T, intent launchapi.LaunchIntentV1, launcher string) ([]byte, []byte, int) {
	t.Helper()
	return fx.prepareJSON(t, fx.intentJSON(t, intent), launcher)
}

func (fx adoptionSmokeFixture) prepareJSON(t *testing.T, intentJSON []byte, launcher string) ([]byte, []byte, int) {
	t.Helper()
	intentPath := filepath.Join(t.TempDir(), "intent.json")
	if err := os.WriteFile(intentPath, intentJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"launch", "--plan", intentPath, "--prepare", "--json", "--launcher", launcher, "--session", fx.session}
	return runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env, args...)
}

func (fx adoptionSmokeFixture) applyEnvelopeJSON(t *testing.T, intent launchapi.LaunchIntentV1, launcher, digest string) []byte {
	t.Helper()
	data, err := json.MarshalIndent(launchapi.ApplyRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Prepare:        fx.request(intent, launcher),
		SubjectDigest:  digest,
		Decisions:      []launchapi.DecisionV1{},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func (fx adoptionSmokeFixture) applyJSONBytes(t *testing.T, applyJSON []byte) ([]byte, []byte, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apply.json")
	if err := os.WriteFile(path, applyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	return runRealAMQWithExit(t, fx.amqBinary, fx.project, fx.env, "launch", "--apply", path, "--json")
}

type liveSeatSnapshot struct {
	LaunchTree         []string
	Binding            string
	LeadConversation   string
	SeniorConversation bool
	Journal            bool
	Panes              int
	SessionIDs         int
}

func (fx adoptionSmokeFixture) liveSeatSnapshot(t *testing.T, socketDir string) liveSeatSnapshot {
	t.Helper()
	logData, _ := os.ReadFile(fx.claudeLog)
	binding, err := os.ReadFile(launch.BindingPath(fx.sessionRoot))
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := os.ReadFile(launch.ConversationPath(fx.sessionRoot, "lead"))
	if err != nil {
		t.Fatal(err)
	}
	_, seniorErr := os.Stat(launch.ConversationPath(fx.sessionRoot, "senior-dev"))
	_, journalErr := os.Stat(launch.JournalPath(fx.sessionRoot))
	return liveSeatSnapshot{
		LaunchTree:         snapshotLaunchTree(t, fx.sessionRoot),
		Binding:            string(binding),
		LeadConversation:   string(conversation),
		SeniorConversation: seniorErr == nil,
		Journal:            journalErr == nil,
		Panes:              countHermeticTmuxPanes(t, socketDir),
		SessionIDs:         strings.Count(string(logData), "--session-id "),
	}
}

func snapshotLaunchTree(t *testing.T, sessionRoot string) []string {
	t.Helper()
	root := filepath.Join(sessionRoot, "meta", "launch")
	var names []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		base := filepath.Base(rel)
		if base == "lease.json" || strings.HasSuffix(base, ".lock") {
			return nil
		}
		entry := rel + "\t" + info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += "\t" + snapshotTreeDigestBytes(data)
		}
		names = append(names, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func snapshotTreeDigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func countHermeticTmuxPanes(t *testing.T, socketDir string) int {
	t.Helper()
	cmd := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_pid}")
	cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+socketDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list hermetic tmux panes: %v\n%s", err, output)
	}
	return len(strings.Fields(string(output)))
}

func injectParticipantJSONField(t *testing.T, intentJSON []byte, index int, key string, value any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(intentJSON, &doc); err != nil {
		t.Fatal(err)
	}
	participants, ok := doc["participants"].([]any)
	if !ok || index >= len(participants) {
		t.Fatalf("participants[%d] missing", index)
	}
	participant, ok := participants[index].(map[string]any)
	if !ok {
		t.Fatalf("participants[%d] is not an object", index)
	}
	participant[key] = value
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func injectPrepareJSONField(t *testing.T, applyJSON []byte, key string, value any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(applyJSON, &doc); err != nil {
		t.Fatal(err)
	}
	prepare, ok := doc["prepare"].(map[string]any)
	if !ok {
		t.Fatal("apply envelope missing prepare object")
	}
	prepare[key] = value
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func assertAdoptionDecodeRejection(t *testing.T, exit int, stdout, stderr []byte, needle string) {
	t.Helper()
	if exit != ExitUsage {
		t.Fatalf("decode rejection exit=%d want %d stdout=%s stderr=%s", exit, ExitUsage, stdout, stderr)
	}
	combined := string(stdout) + string(stderr)
	if !strings.Contains(combined, "not allowed") {
		t.Fatalf("decode rejection missing adapter refusal: stdout=%s stderr=%s", stdout, stderr)
	}
	if !strings.Contains(combined, needle) {
		t.Fatalf("decode rejection did not name %q: stdout=%s stderr=%s", needle, stdout, stderr)
	}
}

func assertAdoptionArgumentAdmitted(t *testing.T, exit int, stdout, stderr []byte, handle, flag, value string) {
	t.Helper()
	if exit != ExitActionRequired {
		t.Fatalf("admitted argument exit=%d want %d stdout=%s stderr=%s", exit, ExitActionRequired, stdout, stderr)
	}
	var result launchapi.PrepareResultV1
	decodeRealLaunchJSON(t, stdout, &result)
	if result.Outcome != "action_required" || result.Reason != "untrusted_config_digest" {
		t.Fatalf("admitted argument did not reach trust gate: outcome=%q reason=%q", result.Outcome, result.Reason)
	}
	for _, participant := range result.Preview.Participants {
		if participant.Handle != handle {
			continue
		}
		if participant.Command == nil {
			t.Fatalf("admitted argument participant %q has no command", handle)
		}
		matches := 0
		for i := 0; i+1 < len(participant.Command.Argv); i++ {
			if participant.Command.Argv[i] == flag && participant.Command.Argv[i+1] == value {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("admitted argument pair %q %q occurs %d times in %q", flag, value, matches, participant.Command.Argv)
		}
		return
	}
	t.Fatalf("admitted argument participant %q missing from preview", handle)
}

func assertAdoptionUnknownField(t *testing.T, exit int, stdout, stderr []byte, field string) {
	t.Helper()
	if exit != ExitUsage {
		t.Fatalf("unknown-field rejection exit=%d want %d stdout=%s stderr=%s", exit, ExitUsage, stdout, stderr)
	}
	combined := string(stdout) + string(stderr)
	if !strings.Contains(combined, "unknown field") || !strings.Contains(combined, field) {
		t.Fatalf("unknown-field rejection missing %q: stdout=%s stderr=%s", field, stdout, stderr)
	}
}
