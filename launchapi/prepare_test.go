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
	wantCapabilities := []ProviderCapabilitiesV1{{
		Provider: "claude", GrammarVersion: 2, VerifiedProviderVersion: "2.1.233",
		AllowedArgumentForms: []string{"--allowedTools", "-n", "--name"}, ConfigOverrides: []ConfigOverrideCapabilityV1{},
		InitialInputKinds: []InitialInputKindV1{InitialInputArgument},
	}}
	if !reflect.DeepEqual(first.Preview.Capabilities, wantCapabilities) {
		t.Fatalf("static capabilities = %#v, want %#v", first.Preview.Capabilities, wantCapabilities)
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

func TestPrepareWrapperComposesVerbatimArgvAndChangesAllDigests(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	baseline, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := writePublicTestWrapper(t)
	runnable := &fixture.request.Intent.Participants[0]
	runnable.Wrapper = &WrapperV1{Executable: wrapper, Args: []string{"--profile", "lead"}}
	runnable.InitialInput = &InitialInputV1{Kind: InitialInputArgument, Text: "bootstrap"}
	wrapped, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	argv := wrapped.Preview.Participants[0].Command.Argv
	if len(argv) < 5 || !slices.Equal(argv[:4], []string{wrapper, "--profile", "lead", runnable.Executable}) || argv[len(argv)-1] != "bootstrap" {
		t.Fatalf("wrapped preview argv = %#v", argv)
	}
	if wrapped.PlanDigest == baseline.PlanDigest || wrapped.TrustDigest == baseline.TrustDigest || wrapped.SubjectDigest == baseline.SubjectDigest {
		t.Fatalf("wrapper digest changes = plan:%t trust:%t subject:%t", wrapped.PlanDigest != baseline.PlanDigest, wrapped.TrustDigest != baseline.TrustDigest, wrapped.SubjectDigest != baseline.SubjectDigest)
	}
}

func TestPrepareRejectsProjectContainedWrapperAndAcceptsOutside(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	inside := filepath.Join(fixture.request.Target.ProjectRoot, "wrapper")
	if err := os.WriteFile(inside, []byte("wrapper"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.request.Intent.Participants[0].Wrapper = &WrapperV1{Executable: inside}
	if _, err := Prepare(context.Background(), fixture.request); err == nil || !strings.Contains(err.Error(), internallaunch.WrapperProjectContainedCode) {
		t.Fatalf("project-contained wrapper error = %v, want %s", err, internallaunch.WrapperProjectContainedCode)
	}

	fixture.request.Intent.Participants[0].Wrapper = &WrapperV1{Executable: writePublicTestWrapper(t)}
	if _, err := Prepare(context.Background(), fixture.request); err != nil {
		t.Fatalf("outside wrapper rejected: %v", err)
	}
}

func TestPrepareRejectsUnauthorizedBaseRootRelationsWithoutWrites(t *testing.T) {
	tests := []struct {
		name  string
		want  string
		build func(t *testing.T, root string) (configuredRoot, baseRoot, sessionRoot string)
	}{
		{
			name: "sibling base", want: "base_root_unauthorized",
			build: func(t *testing.T, root string) (string, string, string) {
				configured := filepath.Join(root, "configured")
				sibling := filepath.Join(root, "sibling")
				for _, path := range []string{configured, sibling} {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				return configured, sibling, filepath.Join(sibling, "collab")
			},
		},
		{
			name: "missing configured root plus profile", want: "base_root_unauthorized",
			build: func(_ *testing.T, root string) (string, string, string) {
				configured := filepath.Join(root, "missing-configured")
				profile := filepath.Join(configured, "profile-a")
				return configured, profile, filepath.Join(profile, "collab")
			},
		},
		{
			name: "nested session", want: "base_root_relation_invalid",
			build: func(t *testing.T, root string) (string, string, string) {
				configured := filepath.Join(root, "configured")
				profile := filepath.Join(configured, "profile-a")
				if err := os.MkdirAll(profile, 0o700); err != nil {
					t.Fatal(err)
				}
				return configured, profile, filepath.Join(profile, "nested", "collab")
			},
		},
		{
			name: "symlink profile escape", want: "base_root_relation_invalid",
			build: func(t *testing.T, root string) (string, string, string) {
				configured := filepath.Join(root, "configured")
				escape := filepath.Join(root, "escape")
				if err := os.Mkdir(configured, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(escape, 0o700); err != nil {
					t.Fatal(err)
				}
				profile := filepath.Join(configured, "profile-a")
				if err := os.Symlink(escape, profile); err != nil {
					t.Fatal(err)
				}
				return configured, profile, filepath.Join(profile, "collab")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicPrepareFixture(t, false)
			root, err := filepath.EvalSymlinks(fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			project, err := filepath.EvalSymlinks(fixture.request.Target.ProjectRoot)
			if err != nil {
				t.Fatal(err)
			}
			configured, base, session := test.build(t, root)
			data, err := json.Marshal(map[string]string{"root": configured})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, ".amqrc"), append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			fixture.request.Target = TargetV1{ProjectRoot: project, BaseRoot: base, SessionRoot: session, Session: "collab"}
			fixture.request.Intent.Participants = []ParticipantV1{{Handle: "operator", Runnable: false}}
			before := snapshotTestTree(t, fixture.root)

			result, err := Prepare(context.Background(), fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != PrepareOutcomeUnsupported || result.Reason != test.want {
				t.Fatalf("refusal = %#v", result)
			}
			if len(result.PlannedWrites) != 0 || len(result.RequiredActions) != 0 {
				t.Fatalf("refusal advertised mutations or decisions: %#v", result)
			}
			if after := snapshotTestTree(t, fixture.root); after != before {
				t.Fatalf("Prepare refusal changed filesystem: before=%s after=%s", before, after)
			}
		})
	}
}

func TestBaseRootOmittedMatchesV061ExactRoot(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	unrelated := filepath.Join(fixture.root, "unrelated-mail")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]string{"root": unrelated})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.request.Target.ProjectRoot, ".amqrc"), append(config, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	omitted, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Outcome == PrepareOutcomeUnsupported || omitted.Reason == "base_root_unauthorized" || omitted.Reason == "base_root_relation_invalid" {
		t.Fatalf("omitted base_root consulted new authority: %#v", omitted)
	}
	if len(omitted.PlannedWrites) != 0 || omitted.Preview.Target.BaseRoot != "" {
		t.Fatalf("omitted base_root changed legacy result: %#v", omitted)
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

func writePublicTestWrapper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seat-wrapper")
	if err := os.WriteFile(path, []byte("wrapper"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
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
