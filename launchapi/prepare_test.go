package launchapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestPrepareIsZeroWriteAndDeterministic(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	before := snapshotTestTree(t, fixture.root)

	first, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotTestTree(t, fixture.root)
	if before != after {
		t.Fatalf("Prepare changed filesystem snapshot: before %s, after %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "provider-probed")); !os.IsNotExist(err) {
		t.Fatalf("Prepare executed the caller provider during its zero-write phase: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated Prepare changed result:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if first.Outcome != PrepareOutcomeActionRequired || first.Reason != "untrusted_config_digest" {
		t.Fatalf("outcome/reason = %q/%q", first.Outcome, first.Reason)
	}
	if first.SubjectSchema != SubjectSchemaV2 {
		t.Fatalf("new Prepare subject schema = %d", first.SubjectSchema)
	}
	if first.Preview.Capabilities == nil || len(first.Preview.Capabilities) != 0 {
		t.Fatalf("capability skeleton must be a deny-by-default empty array: %#v", first.Preview.Capabilities)
	}
	if len(first.RequiredActions) != 1 {
		t.Fatalf("required actions = %#v", first.RequiredActions)
	}
	action := first.RequiredActions[0]
	if action.Kind != RequiredActionTrustConfirmation ||
		!slices.Equal(action.AllowedDecisions, []DecisionChoiceV1{DecisionTrustExactSubject, DecisionDeny}) ||
		action.ActionID == "" {
		t.Fatalf("trust action = %#v", action)
	}
	for _, digest := range []string{first.PlanDigest, first.TrustDigest, first.SubjectDigest, action.ActionID} {
		if err := ValidateDigest(digest); err != nil {
			t.Fatalf("digest %q: %v", digest, err)
		}
	}
	if first.PlanDigest == first.TrustDigest || first.TrustDigest == first.SubjectDigest || first.PlanDigest == first.SubjectDigest {
		t.Fatalf("distinct digest contract collapsed: %#v", first)
	}
	command := first.Preview.Participants[0].Command
	if command == nil || !slices.Contains(command.Argv, "${launch_nonce}") || slices.Contains(command.Argv, prepareTestPlaceholderUUID) {
		t.Fatalf("public command leaked runtime identity or omitted placeholder: %#v", command)
	}
	if command.Cwd != fixture.siblingCwd {
		t.Fatalf("compiled sibling cwd = %q, want %q", command.Cwd, fixture.siblingCwd)
	}
	if !slices.Equal(first.Preview.Roster.Present, []string{"claude"}) || len(first.Preview.Roster.Missing) != 0 {
		t.Fatalf("roster = %#v", first.Preview.Roster)
	}
}

func TestPrepareParticipantOnlyAbsentSessionDoesNotProvision(t *testing.T) {
	fixture := newPublicPrepareFixture(t, false)
	fixture.request.Intent.Participants = []ParticipantV1{{Handle: "operator", Runnable: false}}
	before := snapshotTestTree(t, fixture.root)

	result, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture.request.Target.SessionRoot); !os.IsNotExist(err) {
		t.Fatalf("Prepare provisioned absent session or returned unexpected stat error: %v", err)
	}
	if after := snapshotTestTree(t, fixture.root); after != before {
		t.Fatalf("participant-only Prepare changed filesystem: before %s, after %s", before, after)
	}
	if result.Outcome != PrepareOutcomeReady || len(result.RequiredActions) != 0 {
		t.Fatalf("participant-only result = %#v", result)
	}
	if !slices.Equal(result.Preview.Roster.Missing, []string{"operator"}) || result.Preview.Participants[0].Command != nil {
		t.Fatalf("participant-only preview = %#v", result.Preview)
	}
	for _, digest := range []string{result.PlanDigest, result.TrustDigest, result.SubjectDigest} {
		if err := ValidateDigest(digest); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareUnavailableLauncherIsTypedAndDoesNotRequestTrust(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	launcher := unavailableManagedLauncher(t)
	fixture.request.Launcher = launcher
	result, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != PrepareOutcomeUnsupported || result.Reason != "launcher_not_available" || result.Preview.Backend != launcher {
		t.Fatalf("unavailable launcher result = %#v", result)
	}
	if len(result.RequiredActions) != 0 {
		t.Fatalf("unavailable launcher requested decisions for an inapplicable plan: %#v", result.RequiredActions)
	}
}

func TestPrepareCompilesAdapterBypassArguments(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	fixture.request.Intent.Participants[0].Args = []string{"--model", "test-model", "--dangerously-skip-permissions"}

	result, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	command := result.Preview.Participants[0].Command
	if command == nil || !slices.Contains(command.Argv, "--dangerously-skip-permissions") {
		t.Fatalf("compiled bypass command = %#v", command)
	}
}

func TestPrepareReportsConfiguredOnlyExtraMailbox(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	config := []byte(`{"version":1,"agents":["claude","operator"]}`)
	if err := os.WriteFile(filepath.Join(fixture.request.Target.SessionRoot, "meta", "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Preview.Roster.Extra, []string{"operator"}) {
		t.Fatalf("configured-only extras = %v", result.Preview.Roster.Extra)
	}
	if len(result.Observations) != 2 || result.Observations[1].Handle != "operator" || result.Observations[1].Mailbox == "present" {
		t.Fatalf("configured-only observation = %#v", result.Observations)
	}
}

func TestPrepareSubjectDigestTracksFrozenInputs(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	baseline, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}

	optionsChanged := fixture.request
	optionsChanged.Intent.Participants = slices.Clone(fixture.request.Intent.Participants)
	participant := optionsChanged.Intent.Participants[0]
	execution := *participant.Execution
	execution.NoGitignore = !execution.NoGitignore
	participant.Execution = &execution
	optionsChanged.Intent.Participants[0] = participant
	withOptions, err := Prepare(context.Background(), optionsChanged)
	if err != nil {
		t.Fatal(err)
	}
	if withOptions.PlanDigest != baseline.PlanDigest || withOptions.TrustDigest != baseline.TrustDigest || withOptions.SubjectDigest == baseline.SubjectDigest {
		t.Fatalf("option-only digest change = plan %t trust %t subject %t",
			withOptions.PlanDigest != baseline.PlanDigest,
			withOptions.TrustDigest != baseline.TrustDigest,
			withOptions.SubjectDigest != baseline.SubjectDigest)
	}

	otherBase := filepath.Join(fixture.root, "other-mail")
	otherSession := filepath.Join(otherBase, "collab")
	if err := os.MkdirAll(otherBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureRootDirs(otherSession); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(otherSession, "claude"); err != nil {
		t.Fatal(err)
	}
	rootChanged := fixture.request
	rootChanged.Target.SessionRoot = otherSession
	withRoot, err := Prepare(context.Background(), rootChanged)
	if err != nil {
		t.Fatal(err)
	}
	if withRoot.PlanDigest != baseline.PlanDigest || withRoot.TrustDigest == baseline.TrustDigest || withRoot.SubjectDigest == baseline.SubjectDigest {
		t.Fatalf("root-only digest change = plan %t trust %t subject %t",
			withRoot.PlanDigest != baseline.PlanDigest,
			withRoot.TrustDigest != baseline.TrustDigest,
			withRoot.SubjectDigest != baseline.SubjectDigest)
	}
}

func TestPrepareSubjectDigestTracksCwdIdentityAndRosterObservation(t *testing.T) {
	t.Run("cwd physical identity", func(t *testing.T) {
		fixture := newPublicPrepareFixture(t, true)
		first, err := Prepare(context.Background(), fixture.request)
		if err != nil {
			t.Fatal(err)
		}
		cwd := fixture.request.Intent.Participants[0].Cwd.Path
		if err := os.Rename(cwd, cwd+"-detached"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
		second, err := Prepare(context.Background(), fixture.request)
		if err != nil {
			t.Fatal(err)
		}
		if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest || first.SubjectDigest == second.SubjectDigest {
			t.Fatalf("cwd identity change = plan:%t trust:%t subject:%t",
				first.PlanDigest != second.PlanDigest,
				first.TrustDigest != second.TrustDigest,
				first.SubjectDigest != second.SubjectDigest)
		}
	})

	t.Run("roster observation", func(t *testing.T) {
		fixture := newPublicPrepareFixture(t, true)
		first, err := Prepare(context.Background(), fixture.request)
		if err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(fixture.request.Target.SessionRoot, "operator"); err != nil {
			t.Fatal(err)
		}
		second, err := Prepare(context.Background(), fixture.request)
		if err != nil {
			t.Fatal(err)
		}
		if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest || first.SubjectDigest == second.SubjectDigest {
			t.Fatalf("roster-only change = plan:%t trust:%t subject:%t",
				first.PlanDigest != second.PlanDigest,
				first.TrustDigest != second.TrustDigest,
				first.SubjectDigest != second.SubjectDigest)
		}
		if !slices.Equal(second.Preview.Roster.Extra, []string{"operator"}) {
			t.Fatalf("extra roster = %v", second.Preview.Roster.Extra)
		}
	})
}

func TestPrepareTrustObservationChangesOnlySubjectAndActions(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	first, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	store, err := internallaunch.OpenTrustStore(filepath.Join(fixture.root, "state", "amq"), fixture.request.Target.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace(internallaunch.TrustRecord{SemanticDigest: first.TrustDigest}); err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest || first.SubjectDigest == second.SubjectDigest {
		t.Fatalf("trust-only change = plan:%t trust:%t subject:%t",
			first.PlanDigest != second.PlanDigest,
			first.TrustDigest != second.TrustDigest,
			first.SubjectDigest != second.SubjectDigest)
	}
	if len(first.RequiredActions) != 1 || len(second.RequiredActions) != 0 || second.Outcome != PrepareOutcomeReady {
		t.Fatalf("trust actions before=%#v after=%#v outcome=%s", first.RequiredActions, second.RequiredActions, second.Outcome)
	}
}

const prepareTestPlaceholderUUID = "00000000-0000-4000-8000-000000000000"

type publicPrepareFixture struct {
	root       string
	siblingCwd string
	request    PrepareRequestV1
}

func unavailableManagedLauncher(t *testing.T) string {
	t.Helper()
	for _, name := range []string{internallaunch.LauncherCMux, internallaunch.LauncherGhostty} {
		backend := internallaunch.DefaultBackends()[name]
		if backend != nil && !backend.Detect().Available {
			return name
		}
	}
	t.Skip("cmux and ghostty are both available on this host")
	return ""
}

func newPublicPrepareFixture(t *testing.T, existingSession bool) publicPrepareFixture {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	sibling := filepath.Join(root, "sibling-worktree")
	base := filepath.Join(root, "mail")
	session := filepath.Join(base, "collab")
	for _, path := range []string{project, sibling, base} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if existingSession {
		if err := fsq.EnsureRootDirs(session); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(session, "claude"); err != nil {
			t.Fatal(err)
		}
	}
	executable := buildPrepareTestProvider(t, root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("AMQ_PREPARE_PROBE_MARKER", filepath.Join(root, "provider-probed"))
	canonicalSibling, err := filepath.EvalSymlinks(sibling)
	if err != nil {
		t.Fatal(err)
	}
	return publicPrepareFixture{
		root: root, siblingCwd: canonicalSibling,
		request: PrepareRequestV1{
			RequestVersion: RequestVersionV1,
			Target:         TargetV1{ProjectRoot: project, SessionRoot: session, Session: "collab"},
			Launcher:       "commands",
			Intent: LaunchIntentV1{IntentVersion: IntentVersionV1, Participants: []ParticipantV1{{
				Handle: "claude", Runnable: true, Executable: executable,
				Args:       []string{"--model", "test-model"},
				Cwd:        &WorkingDirectoryV1{Kind: WorkingDirectoryAbsolute, Path: sibling},
				EnvOverlay: map[string]string{"LANG": "C"}, ResumePolicy: ResumePolicyResume,
				Execution: &ExecutionOptionsV1{Wake: WakeOptionsV1{Mode: WakeEnabled}},
			}}},
		},
	}
}

func buildPrepareTestProvider(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "provider")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "main.go")
	program := `package main
import ("fmt"; "os")
func main() {
	if marker := os.Getenv("AMQ_PREPARE_PROBE_MARKER"); marker != "" { _ = os.WriteFile(marker, []byte("probed"), 0600) }
	if len(os.Args) == 2 && os.Args[1] == "--version" { fmt.Println("1.0.0"); return }
	if len(os.Args) == 2 && os.Args[1] == "--help" { fmt.Println("--session-id <uuid> --resume [value]"); return }
	os.Exit(2)
}`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(dir, name)
	command := exec.Command("go", "build", "-o", executable, source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build provider helper: %v\n%s", err, output)
	}
	return executable
}

func snapshotTestTree(t *testing.T, root string) string {
	t.Helper()
	var snapshot bytes.Buffer
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&snapshot, "%s\x00%s\x00%04o\x00", filepath.ToSlash(relative), entry.Type(), info.Mode().Perm())
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot.Write(data)
		}
		snapshot.WriteByte(0)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(snapshot.Bytes())
	return hex.EncodeToString(sum[:])
}

func TestPrepareResultDoesNotContainInternalPlanFields(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	result, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"adapter_mode", "launch_nonce", "conversation_id", "dynamic_argv",
		"conversation_identity_digest", "execution_identity_digest", "cwd_identity",
	} {
		if strings.Contains(string(raw), `"`+forbidden+`"`) {
			t.Errorf("Prepare result exposed %s: %s", forbidden, raw)
		}
	}
}
