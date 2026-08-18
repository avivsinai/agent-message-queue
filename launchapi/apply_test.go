package launchapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestApplyProvisionsMultiSeatRosterAtomically(t *testing.T) {
	fixture := newPublicPrepareFixture(t, false)
	injector := filepath.Join(t.TempDir(), "injector")
	if err := os.WriteFile(injector, []byte("injector"), 0o700); err != nil {
		t.Fatal(err)
	}
	runnable := fixture.request.Intent.Participants[0]
	runnable.Execution = &ExecutionOptionsV1{
		RequireWake: true, NoGitignore: true,
		Wake: WakeOptionsV1{Mode: WakeEnabled, Injector: &InjectorOptionsV1{Mode: InjectorRaw, Via: injector, Args: []string{"send"}}},
		Integrations: IntegrationsV1{Symphony: &SymphonyOptionsV1{
			Events: []SymphonyEvent{SymphonyAfterCreate, SymphonyBeforeRun, SymphonyAfterRun, SymphonyBeforeRemove}, WorkspaceKey: "team-17",
		}},
	}
	reviewer := runnable
	reviewer.Handle = "reviewer"
	fixture.request.Intent.Participants = []ParticipantV1{
		{Handle: "operator", Runnable: false}, runnable, reviewer,
	}
	prepared, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	decisions := decisionsForPreparedActions(prepared)
	result, err := Apply(context.Background(), ApplyRequestV1{
		RequestVersion: RequestVersionV1, Prepare: fixture.request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: decisions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "action_required" || result.ReasonCode != "commands_emitted" || len(result.Commands) != 2 {
		t.Fatalf("commands Apply result = %#v", result)
	}
	if result.SubjectDigest != prepared.SubjectDigest {
		t.Fatalf("Apply authorization subject = %q, want accepted %q", result.SubjectDigest, prepared.SubjectDigest)
	}
	if !slices.Equal(result.Roster.Desired, []string{"claude", "operator", "reviewer"}) ||
		!slices.Equal(result.Roster.Present, result.Roster.Desired) || len(result.Roster.Missing) != 0 || len(result.Roster.Extra) != 0 {
		t.Fatalf("published Apply roster = %#v", result.Roster)
	}
	for _, handle := range []string{"operator", "claude", "reviewer"} {
		for _, leaf := range fsq.RequiredMailboxLeaves() {
			info, err := os.Stat(filepath.Join(fixture.request.Target.SessionRoot, "agents", handle, filepath.FromSlash(string(leaf))))
			if err != nil || !info.IsDir() {
				t.Fatalf("published mailbox %s/%s: info=%v err=%v", handle, leaf, info, err)
			}
		}
	}
	identity, err := fsq.SnapshotDeliveryRoot(fixture.request.Target.SessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(fixture.request.Target.SessionRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	wantOptions := toInternalExecutionOptions(*runnable.Execution)
	wantOptions = internallaunch.CanonicalExecutionOptions(&wantOptions)
	for _, handle := range []string{"claude", "reviewer"} {
		ticket, err := internallaunch.LoadExecutionTicket(root, handle)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ticket.Execution, &wantOptions) {
			t.Fatalf("%s ticket execution options = %#v, want %#v", handle, ticket.Execution, wantOptions)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(fixture.request.Target.SessionRoot))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".amq-session-") {
			t.Fatalf("Apply leaked staging child %q", entry.Name())
		}
	}
}

func TestApplyResultMapsEvidenceRefsAndNonContractFailureDetail(t *testing.T) {
	observed := time.Date(2026, 8, 17, 3, 4, 5, 0, time.UTC)
	result := fromInternalApplyResult(internallaunch.ApplyResult{FailureDetail: "create tmux session: pane exited", Evidence: []internallaunch.EvidenceRef{{
		EvidenceVersion: 1, ID: "sha256:" + strings.Repeat("a", 64), Kind: internallaunch.EvidenceProviderCapture,
		SHA256: "sha256:" + strings.Repeat("a", 64), ObservedAt: observed,
	}}})
	if len(result.Evidence) != 1 || result.Evidence[0].Kind != "provider_capture" || result.Evidence[0].ObservedAt != observed || result.Evidence[0].ID != result.Evidence[0].SHA256 {
		t.Fatalf("public evidence mapping = %#v", result.Evidence)
	}
	if result.FailureDetail != "create tmux session: pane exited" {
		t.Fatalf("failure detail = %q", result.FailureDetail)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("pane exited")) || bytes.Contains(encoded, []byte("failure_detail")) {
		t.Fatalf("non-contract failure detail leaked into DTO JSON: %s", encoded)
	}
}

func TestConcurrentApplyPublishesOneInitializedSession(t *testing.T) {
	fixture := newPublicPrepareFixture(t, false)
	fixture.request.Intent.Participants = []ParticipantV1{{Handle: "operator", Runnable: false}}
	prepared, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequestV1{
		RequestVersion: RequestVersionV1, Prepare: fixture.request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: []DecisionV1{},
	}
	start := make(chan struct{})
	results := make(chan ApplyResultV1, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, applyErr := Apply(context.Background(), request)
			results <- result
			errors <- applyErr
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errors)
	for applyErr := range errors {
		if applyErr != nil {
			t.Fatal(applyErr)
		}
	}
	counts := map[string]int{}
	for result := range results {
		counts[result.Outcome+":"+result.ReasonCode]++
	}
	if counts["provisioned_no_runnable:"] != 1 || counts["action_required:subject_changed"] != 1 {
		t.Fatalf("concurrent Apply outcomes = %#v", counts)
	}
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		if info, err := os.Stat(filepath.Join(fixture.request.Target.SessionRoot, "agents", "operator", filepath.FromSlash(string(leaf)))); err != nil || !info.IsDir() {
			t.Fatalf("single-winner mailbox %s: info=%v err=%v", leaf, info, err)
		}
	}
}

func TestPrepareIsZeroWriteAndApplyRejectsStaleSubject(t *testing.T) {
	t.Run("existing session", func(t *testing.T) {
		fixture := newPublicPrepareFixture(t, true)
		prepared, err := Prepare(context.Background(), fixture.request)
		if err != nil {
			t.Fatal(err)
		}
		changed := fixture.request
		changed.Intent.Participants = slices.Clone(changed.Intent.Participants)
		participant := changed.Intent.Participants[0]
		execution := *participant.Execution
		execution.NoGitignore = !execution.NoGitignore
		participant.Execution = &execution
		changed.Intent.Participants[0] = participant
		before := applyTreeFingerprint(t, fixture.root, nil)
		result, err := Apply(context.Background(), ApplyRequestV1{
			RequestVersion: RequestVersionV1, Prepare: changed,
			SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: decisionsForPreparedActions(prepared),
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome != "action_required" || result.ReasonCode != "subject_changed" {
			t.Fatalf("stale Apply result = %#v", result)
		}
		allowed := map[string]bool{
			"mail/collab/meta/launch":            true,
			"mail/collab/meta/launch/lease.lock": true,
		}
		after := applyTreeFingerprint(t, fixture.root, allowed)
		if before != after {
			t.Fatalf("stale Apply changed durable state: before=%s after=%s", before, after)
		}
		if _, err := os.Stat(filepath.Join(fixture.request.Target.SessionRoot, "meta", "launch", "lease.json")); !os.IsNotExist(err) {
			t.Fatalf("stale Apply retained lease record: %v", err)
		}
	})

	t.Run("absent session", func(t *testing.T) {
		fixture := newPublicPrepareFixture(t, false)
		fixture.request.Intent.Participants = []ParticipantV1{{Handle: "operator", Runnable: false}}
		prepared, err := Prepare(context.Background(), fixture.request)
		if err != nil {
			t.Fatal(err)
		}
		changed := fixture.request
		changed.Intent.Participants = []ParticipantV1{{Handle: "reviewer", Runnable: false}}
		before := applyTreeFingerprint(t, fixture.root, nil)
		result, err := Apply(context.Background(), ApplyRequestV1{
			RequestVersion: RequestVersionV1, Prepare: changed,
			SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: []DecisionV1{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.ReasonCode != "subject_changed" {
			t.Fatalf("absent stale Apply result = %#v", result)
		}
		digest := sha256.Sum256([]byte("collab"))
		lock := "mail/meta/launch/create-" + hex.EncodeToString(digest[:12]) + ".lock"
		allowed := map[string]bool{"mail/meta": true, "mail/meta/launch": true, lock: true}
		if after := applyTreeFingerprint(t, fixture.root, allowed); before != after {
			t.Fatalf("absent stale Apply changed durable state: before=%s after=%s", before, after)
		}
		if _, err := os.Stat(fixture.request.Target.SessionRoot); !os.IsNotExist(err) {
			t.Fatalf("stale Apply published absent session: %v", err)
		}
	})
}

func TestActionRequiredSurvivesProcessRestart(t *testing.T) {
	fixture := newPublicPrepareFixture(t, false)
	fixture.request.Intent.Participants = []ParticipantV1{{Handle: "operator", Runnable: false}}
	prepared, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequestV1{
		RequestVersion: RequestVersionV1, Prepare: fixture.request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: []DecisionV1{},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeApplyRequestV1(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), decoded)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "provisioned_no_runnable" || result.SemanticDigest != result.TrustDigest {
		t.Fatalf("fresh-process Apply result = %#v", result)
	}
	if result.SubjectSchema != SubjectSchemaV2 || len(result.Hints) != 0 {
		t.Fatalf("current Apply schema migration fields = %#v", result)
	}
	if info, err := os.Stat(filepath.Join(fixture.request.Target.SessionRoot, "agents", "operator")); err != nil || !info.IsDir() {
		t.Fatalf("participant-only mailbox: info=%v err=%v", info, err)
	}
}

func TestApplyOmittedSubjectSchemaUsesV1AndRecommendsReprepare(t *testing.T) {
	fixture := newPublicPrepareFixture(t, false)
	fixture.request.Intent.Participants = []ParticipantV1{{Handle: "operator", Runnable: false}}
	internalRequest, dependencies, err := prepareInputs(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	internalRequest.SubjectSchema = internallaunch.SubjectSchemaV1
	legacyInternal, err := internallaunch.Prepare(context.Background(), internalRequest, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	legacy := fromInternalPrepareResult(legacyInternal)
	if legacy.SubjectSchema != SubjectSchemaV1 {
		t.Fatalf("legacy Prepare schema = %d", legacy.SubjectSchema)
	}
	raw, err := json.Marshal(struct {
		RequestVersion int              `json:"request_version"`
		Prepare        PrepareRequestV1 `json:"prepare"`
		SubjectDigest  string           `json:"subject_digest"`
		Decisions      []DecisionV1     `json:"decisions"`
	}{RequestVersionV1, fixture.request, legacy.SubjectDigest, []DecisionV1{}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeApplyRequestV1(raw)
	if err != nil {
		t.Fatal(err)
	}
	if request.SubjectSchema != 0 {
		t.Fatalf("omitted subject_schema decoded as %d", request.SubjectSchema)
	}
	result, err := Apply(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome == "action_required" && result.ReasonCode == "subject_changed" {
		t.Fatalf("legacy Apply spuriously changed subject: %#v", result)
	}
	if result.SubjectSchema != SubjectSchemaV1 || !reflect.DeepEqual(result.Hints, []ResultHintV1{HintReprepareRecommended}) {
		t.Fatalf("legacy Apply migration result = %#v", result)
	}
	upgraded, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.SubjectSchema != SubjectSchemaV2 || upgraded.SubjectDigest == legacy.SubjectDigest {
		t.Fatalf("re-Prepare did not upgrade schema: legacy=%#v upgraded=%#v", legacy, upgraded)
	}
}

func TestApplyReportsRosterDriftWithoutDeletingHistory(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	if err := fsq.EnsureAgentDirs(fixture.request.Target.SessionRoot, "operator"); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"version":1,"agents":["claude","operator"]}`)
	if err := os.WriteFile(filepath.Join(fixture.request.Target.SessionRoot, "meta", "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(fixture.request.Target.SessionRoot, "agents", "operator", "inbox", "cur", "history.json")
	history := []byte("preserved-history")
	if err := os.WriteFile(historyPath, history, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.request.Target.SessionRoot, "agents", "operator", "receipts")); err != nil {
		t.Fatal(err)
	}
	operatorRoot := filepath.Join(fixture.request.Target.SessionRoot, "agents", "operator")
	operatorBefore := applyTreeFingerprint(t, operatorRoot, nil)
	fixture.request.Intent.Participants = []ParticipantV1{
		{Handle: "claude", Runnable: false},
		{Handle: "reviewer", Runnable: false},
	}
	prepared, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), ApplyRequestV1{
		RequestVersion: RequestVersionV1, Prepare: fixture.request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: []DecisionV1{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "provisioned_no_runnable" ||
		!slices.Equal(result.Roster.Desired, []string{"claude", "reviewer"}) ||
		!slices.Equal(result.Roster.Present, []string{"claude", "reviewer"}) ||
		len(result.Roster.Missing) != 0 || !slices.Equal(result.Roster.Extra, []string{"operator"}) {
		t.Fatalf("Apply roster drift = %#v", result.Roster)
	}
	if got, err := os.ReadFile(historyPath); err != nil || !bytes.Equal(got, history) {
		t.Fatalf("removed participant history changed: got=%q err=%v", got, err)
	}
	if operatorAfter := applyTreeFingerprint(t, operatorRoot, nil); operatorAfter != operatorBefore {
		t.Fatalf("Apply repaired or changed removed participant: before=%s after=%s", operatorBefore, operatorAfter)
	}
}

func TestApplyMissingConfigPreservesDiscoveredRosterAuthority(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	if err := fsq.EnsureAgentDirs(fixture.request.Target.SessionRoot, "operator"); err != nil {
		t.Fatal(err)
	}
	fixture.request.Intent.Participants = []ParticipantV1{{Handle: "reviewer", Runnable: false}}
	prepared, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), ApplyRequestV1{
		RequestVersion: RequestVersionV1, Prepare: fixture.request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: []DecisionV1{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "provisioned_no_runnable" || !slices.Equal(result.Roster.Extra, []string{"claude", "operator"}) {
		t.Fatalf("missing-config Apply result = %#v", result)
	}
	data, err := os.ReadFile(filepath.Join(fixture.request.Target.SessionRoot, "meta", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(config.Agents, []string{"claude", "operator", "reviewer"}) {
		t.Fatalf("recovered roster authority = %v", config.Agents)
	}
}

func TestApplyRequiresExactRequestLocalDecisions(t *testing.T) {
	tests := []struct {
		name      string
		decisions func(PrepareResultV1) []DecisionV1
		reason    string
	}{
		{name: "missing", decisions: func(PrepareResultV1) []DecisionV1 { return []DecisionV1{} }, reason: "decisions_incomplete"},
		{name: "unknown action", decisions: func(prepared PrepareResultV1) []DecisionV1 {
			return []DecisionV1{{ActionID: "sha256:" + strings.Repeat("0", 64), Choice: DecisionTrustExactSubject}}
		}, reason: "decision_unknown_action"},
		{name: "disallowed", decisions: func(prepared PrepareResultV1) []DecisionV1 {
			return []DecisionV1{{ActionID: prepared.RequiredActions[0].ActionID, Choice: DecisionFreshOnce}}
		}, reason: "decision_disallowed"},
		{name: "declined", decisions: func(prepared PrepareResultV1) []DecisionV1 {
			return []DecisionV1{{ActionID: prepared.RequiredActions[0].ActionID, Choice: DecisionDeny}}
		}, reason: "decision_declined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicPrepareFixture(t, true)
			prepared, err := Prepare(context.Background(), fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			before := applyTreeFingerprint(t, fixture.root, nil)
			result, err := Apply(context.Background(), ApplyRequestV1{
				RequestVersion: RequestVersionV1, Prepare: fixture.request,
				SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: test.decisions(prepared),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != "action_required" || result.ReasonCode != test.reason {
				t.Fatalf("decision refusal = %#v", result)
			}
			allowed := map[string]bool{
				"mail/collab/meta/launch":            true,
				"mail/collab/meta/launch/lease.lock": true,
			}
			if after := applyTreeFingerprint(t, fixture.root, allowed); before != after {
				t.Fatalf("decision refusal changed durable state: before=%s after=%s", before, after)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "provider-probed")); !os.IsNotExist(err) {
				t.Fatalf("decision refusal executed provider: %v", err)
			}
		})
	}
}

func TestApplyUnsupportedLauncherRefusesBeforeRosterMutation(t *testing.T) {
	fixture := newPublicPrepareFixture(t, true)
	fixture.request.Launcher = unavailableManagedLauncher(t)
	fixture.request.Intent.Participants = append(fixture.request.Intent.Participants, ParticipantV1{Handle: "reviewer", Runnable: false})
	prepared, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	before := applyTreeFingerprint(t, fixture.root, nil)
	result, err := Apply(context.Background(), ApplyRequestV1{
		RequestVersion: RequestVersionV1, Prepare: fixture.request,
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, Decisions: []DecisionV1{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "action_required" || result.ReasonCode != "launcher_not_available" {
		t.Fatalf("unsupported Apply = %#v", result)
	}
	allowed := map[string]bool{
		"mail/collab/meta/launch":            true,
		"mail/collab/meta/launch/lease.lock": true,
	}
	if after := applyTreeFingerprint(t, fixture.root, allowed); before != after {
		t.Fatalf("unsupported Apply changed durable state: before=%s after=%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(fixture.request.Target.SessionRoot, "agents", "reviewer")); !os.IsNotExist(err) {
		t.Fatalf("unsupported Apply provisioned reviewer: %v", err)
	}
}

func decisionsForPreparedActions(prepared PrepareResultV1) []DecisionV1 {
	result := make([]DecisionV1, 0, len(prepared.RequiredActions))
	for _, action := range prepared.RequiredActions {
		result = append(result, DecisionV1{ActionID: action.ActionID, Choice: action.AllowedDecisions[0]})
	}
	return result
}

func applyTreeFingerprint(t *testing.T, root string, allowed map[string]bool) string {
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
		relative = filepath.ToSlash(relative)
		if allowed[relative] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snapshot.WriteString(relative)
		snapshot.WriteByte(0)
		snapshot.WriteString(info.Mode().String())
		snapshot.WriteByte(0)
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
