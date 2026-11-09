package launch

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestExecutionTicketRoundTripAndCAS(t *testing.T) {
	fixture := newExecutionFixture(t)
	lease, err := AcquireLease(fixture.root, "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	execution := &PrepareExecutionOptions{
		RequireWake: true, NoGitignore: true, WakeMode: "enabled",
		InjectorMode: "raw", InjectorVia: fixture.injector, InjectorArgs: []string{"send"},
		SymphonyEvents: []string{"after_create", "before_run", "after_run", "before_remove"}, SymphonyWorkspaceKey: "team-17",
	}
	initialInput := &PlannedInitialInput{Kind: InitialInputArgument, SHA256: initialInputDigest("bootstrap"), ArgvIndex: 3}
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: lease.LaunchNonce(), Mode: AdapterModeMint,
		Provider: ClaudeProvider, ConversationID: lease.LaunchNonce(),
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv:   []string{fixture.provider, "--session-id", lease.LaunchNonce(), "bootstrap"},
		InitialInput: initialInput,
		TargetEnv:    map[string]string{"LANG": "C", "NO_COLOR": "1"},
		Execution:    execution,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExecutionTicket(fixture.root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != ExecutionPending || loaded.TargetEnv["LANG"] != "C" || loaded.EnvDigest == "" ||
		!reflect.DeepEqual(loaded.Execution, execution) || !reflect.DeepEqual(loaded.InitialInput, initialInput) {
		t.Fatalf("loaded ticket = %#v", loaded)
	}
	loaded, err = CompareAndSwapExecutionTicket(fixture.root, lease, "claude", ExecutionPending, ExecutionSpawnAttempted, "spawned")
	if err != nil || loaded.State != ExecutionSpawnAttempted {
		t.Fatalf("pending -> spawn_attempted = %#v, %v", loaded, err)
	}
	loaded, err = CompareAndSwapExecutionTicket(fixture.root, lease, "claude", ExecutionSpawnAttempted, ExecutionAcknowledged, "acknowledged")
	if err != nil || loaded.State != ExecutionAcknowledged {
		t.Fatalf("spawn_attempted -> acknowledged = %#v, %v", loaded, err)
	}
	if _, err := CompareAndSwapExecutionTicket(fixture.root, lease, "claude", ExecutionPending, ExecutionSpawnAttempted, "late"); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("stale CAS error = %v", err)
	}
}

func TestPrepareMintPromotesExactPendingAndExecFailureReverts(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "55555555-5555-4555-8555-555555555555"
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	ticket := mustExecutionTicket(t, fixture, nonce)
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(fixture.root, lease, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CapturePending, LaunchNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	envelope := ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: fixture.provider,
		TargetArgv: ticket.TargetArgv, Environment: []string{"LANG=C"},
	}
	prepared, err := PrepareExecution(fixture.root, "claude", nonce, envelope)
	if err != nil || prepared.State != ExecutionAcknowledged {
		t.Fatalf("prepare = %#v, %v", prepared, err)
	}
	record, err := LoadConversation(fixture.root, "claude")
	if err != nil || record.State != CaptureReady || record.Identity.ID != nonce {
		t.Fatalf("ready conversation = %#v, %v", record, err)
	}
	if err := RevertExecution(fixture.root, "claude", nonce); err != nil {
		t.Fatal(err)
	}
	record, err = LoadConversation(fixture.root, "claude")
	if err != nil || record.State != CapturePending || record.Reason != "spawn_failed" {
		t.Fatalf("reverted conversation = %#v, %v", record, err)
	}
}

func TestPrepareExecutionAcceptsDeclaredWrapperTarget(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "56565656-5656-4565-8565-565656565656"
	wrapper := testWrapper(t)
	targetArgv := []string{wrapper.Executable, "--profile", "lead", fixture.provider, "--session-id", nonce}
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: nonce, Mode: AdapterModeMint, Provider: ClaudeProvider, ConversationID: nonce,
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv: targetArgv, TargetEnv: map[string]string{"LANG": "C"}, Wrapper: wrapper,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(fixture.root, lease, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CapturePending, LaunchNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareExecution(fixture.root, "claude", nonce, ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: wrapper.Executable,
		TargetArgv: targetArgv, Environment: []string{"LANG=C"},
	})
	if err != nil || prepared.State != ExecutionAcknowledged {
		t.Fatalf("PrepareExecution = %#v, %v", prepared, err)
	}
	resolved, err := ResolveExecutionArgv(prepared, nil)
	if err != nil || !slices.Equal(resolved, targetArgv) {
		t.Fatalf("ResolveExecutionArgv = %#v, %v", resolved, err)
	}
	changed := ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: fixture.provider,
		TargetArgv: targetArgv, Environment: []string{"LANG=C"},
	}
	if err := ValidateExecutionEnvelope(fixture.root, prepared, changed); err == nil || !strings.Contains(err.Error(), "execution target changed") {
		t.Fatalf("provider bypassed wrapper: %v", err)
	}
	tampered := prepared
	tampered.TargetArgv = slices.Clone(prepared.TargetArgv)
	tampered.TargetArgv[3] = wrapper.Executable
	changed.ProviderExecutable = wrapper.Executable
	changed.TargetArgv = tampered.TargetArgv
	if err := ValidateExecutionEnvelope(fixture.root, tampered, changed); err == nil || !strings.Contains(err.Error(), "provider target identity changed") {
		t.Fatalf("wrapper redirected inner provider: %v", err)
	}
}

func TestPrepareExecutionRejectsRetargetedProviderWithoutPromotion(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "66666666-6666-4666-8666-666666666666"
	link := filepath.Join(t.TempDir(), "provider-link")
	if err := os.Symlink(fixture.provider, link); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: nonce, Mode: AdapterModeMint, Provider: ClaudeProvider, ConversationID: nonce,
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: link, AMQExecutable: fixture.amq, TargetArgv: []string{link}, TargetEnv: map[string]string{"LANG": "C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(fixture.root, lease, ConversationRecord{Version: ConversationVersion, Handle: "claude", State: CapturePending, LaunchNonce: nonce}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, link); err != nil {
		t.Fatal(err)
	}
	_, err = PrepareExecution(fixture.root, "claude", nonce, ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: link,
		TargetArgv: ticket.TargetArgv, Environment: []string{"LANG=C"},
	})
	if err == nil || !strings.Contains(err.Error(), "provider executable identity changed") {
		t.Fatalf("retarget error = %v", err)
	}
	record, loadErr := LoadConversation(fixture.root, "claude")
	if loadErr != nil || record.State != CapturePending {
		t.Fatalf("conversation mutated = %#v, %v", record, loadErr)
	}
}

func TestPrepareCaptureRecordsAttemptWithoutPromotingConversation(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "88888888-8888-4888-8888-888888888888"
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: nonce, Mode: AdapterModeCapture, Provider: CodexProvider,
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv: []string{fixture.provider}, TargetEnv: map[string]string{"LANG": "C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(fixture.root, lease, ConversationRecord{Version: ConversationVersion, Handle: "claude", State: CapturePending, LaunchNonce: nonce}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareExecution(fixture.root, "claude", nonce, ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: fixture.provider,
		TargetArgv: ticket.TargetArgv, Environment: []string{"LANG=C"},
	})
	if err != nil || prepared.State != ExecutionSpawnAttempted {
		t.Fatalf("capture prepare = %#v, %v", prepared, err)
	}
	record, err := LoadConversation(fixture.root, "claude")
	if err != nil || record.State != CapturePending || record.ExecutionEvidence != nil {
		t.Fatalf("capture conversation = %#v, %v", record, err)
	}
}

type countingProviderAcquirer struct {
	calls int
	id    string
}

func (acquirer *countingProviderAcquirer) Acquire(ticket ExecutionTicket) (CaptureEvidence, error) {
	acquirer.calls++
	if ticket.Provider == CodexProvider {
		holder, err := HolderOf(os.Getpid())
		if err != nil {
			return CaptureEvidence{}, err
		}
		return ObserveCodexDaemonThread(ticket, acquirer.id, filepath.Join(ticket.Cwd, "daemon.sock"), holder)
	}
	return ParseCursorCreateChatEvidence([]byte(acquirer.id+"\n"), ticket.LaunchNonce, ticket.Handle, ticket.ProviderVersion)
}

func TestPreparePreSpawnCaptureReusesAcquisitionAcrossCrashes(t *testing.T) {
	// Pro S1 finding 2: a stable symlink path was rejected even when it
	// resolved to the ticket's bound executable identity.
	for _, provider := range []string{CursorProvider, CodexProvider, "codex_symlink"} {
		for _, crashStage := range []string{"", "evidence_persisted", "identity_acquired", "spawn_attempted"} {
			if provider == CursorProvider && crashStage == "" || provider == "codex_symlink" && crashStage != "" {
				continue
			}
			t.Run(provider+"/"+crashStage, func(t *testing.T) {
				provider := provider
				fixture := newExecutionFixture(t)
				nonce := "91919191-9191-4919-8919-919191919191"
				var ticket ExecutionTicket
				var envelope ExecutionEnvelope
				if provider == CodexProvider || provider == "codex_symlink" {
					if provider == "codex_symlink" {
						real := fixture.provider + "-real"
						if err := os.Rename(fixture.provider, real); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(real, fixture.provider); err != nil {
							t.Fatal(err)
						}
						provider = CodexProvider
					}
					ticket, envelope = seedPendingCodexDaemonExecution(t, fixture, nonce)
				} else {
					ticket, envelope = seedPendingCursorExecution(t, fixture, nonce)
				}
				acquirer := &countingProviderAcquirer{id: testConversationID}
				registrations := 0
				if provider == CodexProvider {
					envelope.RecordIdentity = func(acquired ExecutionTicket) error {
						if acquired.ConversationID != testConversationID || len(acquired.EvidenceRefs) != 1 {
							t.Fatalf("unbound identity registration: %#v", acquired)
						}
						stored, err := LoadExecutionTicket(fixture.root, ticket.Handle)
						if err != nil || stored.ConversationID != acquired.ConversationID || stored.LaunchNonce != nonce {
							t.Fatalf("identity registered outside its durable generation: %#v, %v", stored, err)
						}
						registrations++
						return nil
					}
				}
				crash := errors.New("simulated crash")
				prepared, err := prepareExecution(fixture.root, ticket.Handle, nonce, envelope, acquirer, func(stage string) error {
					if stage == crashStage {
						return crash
					}
					return nil
				})
				if crashStage != "" {
					if !errors.Is(err, crash) {
						t.Fatalf("first prepare error = %v", err)
					}

					// Claude recovery finding 4: a replan must reuse incomplete
					// acquired progress rather than orphan it under a new nonce.
					if provider == CodexProvider && (crashStage == "identity_acquired" || crashStage == "spawn_attempted") {
						nextNonce := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
						other, leaseErr := AcquireLease(fixture.root, nextNonce)
						if leaseErr != nil {
							t.Fatal(leaseErr)
						}
						if err := other.LockHandles(ticket.Handle); err != nil {
							t.Fatal(err)
						}
						next := ticket
						next.LaunchNonce = nextNonce
						writeErr := WriteExecutionTicket(fixture.root, other, next)
						if err := other.Release(); err != nil {
							t.Fatal(err)
						}
						if !errors.Is(writeErr, ErrCodexExecutionRecoveryRequired) {
							t.Fatalf("replacement of acquired native identity = %v", writeErr)
						}
					}
					prepared, err = prepareExecution(fixture.root, ticket.Handle, nonce, envelope, acquirer, nil)
				}
				if err != nil || prepared.State != ExecutionAcknowledged || acquirer.calls != 1 {
					t.Fatalf("prepare = %#v, %v; acquisition calls = %d", prepared, err, acquirer.calls)
				}
				if provider == CodexProvider && registrations == 0 {
					t.Fatal("daemon routing was never registered")
				}
				argv, err := ResolveExecutionArgv(prepared, nil)
				if err != nil || argv[ticket.DynamicArgv[0].Index] != testConversationID {
					t.Fatalf("resolved argv = %#v, %v", argv, err)
				}
				record, err := LoadConversation(fixture.root, ticket.Handle)
				if err != nil || record.State != CaptureReady || record.Identity.Provider != provider || record.Identity.ID != testConversationID || len(record.EvidenceRefs) != 1 || record.ExecutionEvidence.LaunchNonce != nonce {
					t.Fatalf("ready conversation = %#v, %v", record, err)
				}
			})
		}
	}
}

// Pre-merge review found that a lost thread/start reply could leave a
// created native thread and let a retry create a second one for the same ticket.
func TestCodexDaemonDoesNotRepeatAnUncertainThreadStart(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "92929292-9292-4929-8929-929292929292"
	original, envelope := seedPendingCodexDaemonExecution(t, fixture, nonce)
	calls := 0
	lostReply := errors.New("thread/start response lost")
	envelope.AcquireIdentity = func(ticket ExecutionTicket) (CaptureEvidence, error) { calls++; return CaptureEvidence{}, lostReply }
	if _, err := PrepareExecution(fixture.root, "codex", nonce, envelope); !errors.Is(err, lostReply) {
		t.Fatalf("first acquisition = %v", err)
	}
	if _, err := PrepareExecution(fixture.root, "codex", nonce, envelope); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("retry = %v", err)
	}
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("codex"); err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(fixture.root, lease, original); err != nil {
		t.Fatal(err)
	}
	if err := RemoveExecutionTicket(fixture.root, lease, "codex", nonce); err == nil {
		t.Fatal("uncertain execution was removed")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadExecutionTicket(fixture.root, "codex")
	if err != nil || stored.State != ExecutionIdentityRequested || calls != 1 {
		t.Fatalf("uncertain acquisition = %#v, %v; calls=%d", stored, err, calls)
	}
}

func seedPendingCodexDaemonExecution(t *testing.T, fixture executionFixture, nonce string) (ExecutionTicket, ExecutionEnvelope) {
	t.Helper()
	provider := filepath.Join(filepath.Dir(fixture.provider), "codex")
	if err := os.Rename(fixture.provider, provider); err != nil {
		t.Fatal(err)
	}
	plan, err := NewCodexAdapter(provider).PlanFreshDaemon(PlanRequest{Handle: "codex", LaunchNonce: nonce, ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd, AMQExecutable: fixture.amq, ResumePolicy: ResumeEnabled}, filepath.Join(fixture.cwd, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: plan.Handle, LaunchNonce: nonce, Mode: plan.AdapterMode, Provider: CodexProvider, ProviderVersion: CodexDaemonVersion,
		PreSpawnAcquire: plan.PreSpawnAcquire, Backend: CommandsBackendName, Profile: CommandsProfile().Identity(),
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd, ProviderExecutable: provider, AMQExecutable: fixture.amq,
		TargetArgv: plan.Argv, DynamicArgv: plan.DynamicArgv,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if err := lease.LockHandles("codex"); err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(fixture.root, lease, ConversationRecord{Version: ConversationVersion, Handle: "codex", State: CapturePending, ProviderVersion: CodexDaemonVersion, LaunchNonce: nonce}); err != nil {
		t.Fatal(err)
	}
	return ticket, ExecutionEnvelope{Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: provider, TargetArgv: plan.Argv}
}

func TestPrepareCursorFailsClosedOnTamperedAcquisitionEvidence(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "93939393-9393-4939-8939-939393939393"
	_, envelope := seedPendingCursorExecution(t, fixture, nonce)
	acquirer := &countingProviderAcquirer{id: testConversationID}
	crash := errors.New("stop after identity publication")
	_, err := prepareExecution(fixture.root, "cursor", nonce, envelope, acquirer, func(stage string) error {
		if stage == "identity_acquired" {
			return crash
		}
		return nil
	})
	if !errors.Is(err, crash) {
		t.Fatalf("prepare error = %v", err)
	}
	ticket, err := LoadExecutionTicket(fixture.root, "cursor")
	if err != nil {
		t.Fatal(err)
	}
	path := EvidencePath(fixture.session, ticket.EvidenceRefs[0])
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareExecution(fixture.root, "cursor", nonce, envelope, acquirer, nil); err == nil || !strings.Contains(err.Error(), "evidence_corrupt") {
		t.Fatalf("tampered evidence error = %v", err)
	}
	if acquirer.calls != 1 {
		t.Fatalf("tampered retry acquisition calls = %d, want 1", acquirer.calls)
	}
}

func TestRecordCodexNotifyPersistsBeforePromotionAndUsesTicketBackend(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "96969696-9696-4969-8969-969696969696"
	ticket := seedPendingCodexNotifyExecution(t, fixture, nonce)
	codexHome := t.TempDir()
	writeCodexRollout(t, codexHome, testConversationID, ticket.Cwd)
	raw := codexNotifyTestPayload(testConversationID, ticket.Cwd)
	result, err := RecordCodexNotify(fixture.root, "codex", fixture.amq, codexHome, raw)
	if err != nil || result.ConversationID != testConversationID || result.EvidenceRef == "" || result.AlreadyReady {
		t.Fatalf("RecordCodexNotify = %#v, %v", result, err)
	}
	record, err := LoadConversation(fixture.root, "codex")
	if err != nil || record.State != CaptureReady || record.Identity != (ConversationIdentity{Provider: CodexProvider, ID: testConversationID}) ||
		record.ExecutionEvidence == nil || record.ExecutionEvidence.Backend != CommandsBackendName ||
		record.ExecutionEvidence.Profile != CommandsProfile().Identity() || !reflect.DeepEqual(record.EvidenceRefs, []string{result.EvidenceRef}) {
		t.Fatalf("ready conversation = %#v, %v", record, err)
	}
	if _, _, err := ReadEvidence(fixture.root, result.EvidenceRef); err != nil {
		t.Fatal(err)
	}
	second, err := RecordCodexNotify(fixture.root, "codex", fixture.amq, codexHome, raw)
	if err != nil || !second.AlreadyReady || second.EvidenceRef != result.EvidenceRef {
		t.Fatalf("idempotent notify = %#v, %v", second, err)
	}
}

// TestResolveCodexExecutionFillsIdentityFromEnvironment covers the managed
// half of bead agent-message-queue-vd4.1: a managed Codex seat's shell tool
// lost AM_* under shell_environment_policy.inherit = "core", so its amq
// commands searched other projects for a root.
func TestResolveCodexExecutionFillsIdentityFromEnvironment(t *testing.T) {
	fixture := newExecutionFixture(t)
	ticket := seedPendingCodexNotifyExecution(t, fixture, "97979797-9797-4979-8979-979797979797")
	env := []string{"AM_ROOT=" + fixture.session, "AM_BASE_ROOT=" + fixture.project, "AM_SESSION=", "AM_ME=7",
		"AM_ROOT_ID=r\\1", "AM_BASE_ROOT_ID=b\"1"}
	argv, err := ResolveExecutionArgv(ticket, env)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`shell_environment_policy.set.AM_ROOT="` + fixture.session + `"`, `shell_environment_policy.set.AM_SESSION=""`,
		`shell_environment_policy.set.AM_ME="7"`, `shell_environment_policy.set.AM_ROOT_ID="r\\1"`,
		`shell_environment_policy.set.AM_BASE_ROOT_ID="b\"1"`,
	} {
		if !slices.Contains(argv, want) {
			t.Fatalf("resolved argv %q lacks %s", argv, want)
		}
	}
	if _, err := ResolveExecutionArgv(ticket, env[:5]); err == nil {
		t.Fatal("a seat without AM_BASE_ROOT_ID resolved")
	}
	// Pre-merge review: the trust digest names only a slot's kind, so a plan
	// whose slots all set AM_ME digested like the real one and ran with no
	// AM_ROOT.
	duplicate := AgentPlan{Handle: "codex", Argv: slices.Clone(ticket.TargetArgv), DynamicArgv: ticket.DynamicArgv,
		Cwd: fixture.cwd, AdapterMode: AdapterModeCapture, ResumePolicy: ResumeFresh}
	if err := duplicate.validate(); err != nil {
		t.Fatalf("valid identity slots refused: %v", err)
	}
	for _, dynamic := range duplicate.DynamicArgv {
		duplicate.Argv[dynamic.Index] = codexIdentitySetPrefix + "AM_ME=" + codexIdentityPlaceholder
	}
	if err := duplicate.validate(); err == nil {
		t.Fatal("a plan whose identity slots repeat AM_ME validated")
	}
}

// #1174 review: a CODEX_HOME whose path holds glob syntax, such as
// "[codex]", must still find the seat's rollout.
func TestRecordCodexNotifyFindsRolloutUnderGlobSyntaxHome(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "98989898-9898-4989-8989-989898989898"
	ticket := seedPendingCodexNotifyExecution(t, fixture, nonce)
	codexHome := filepath.Join(t.TempDir(), "[codex]")
	writeCodexRollout(t, codexHome, testConversationID, ticket.Cwd)
	result, err := RecordCodexNotify(fixture.root, "codex", fixture.amq, codexHome, codexNotifyTestPayload(testConversationID, ticket.Cwd))
	if err != nil || result.NotTopLevel || result.ConversationID != testConversationID {
		t.Fatalf("RecordCodexNotify = %#v, %v", result, err)
	}
}

func seedPendingCodexNotifyExecution(t *testing.T, fixture executionFixture, nonce string) ExecutionTicket {
	t.Helper()
	notify, err := codexNotifyOverride(PlanRequest{
		AMQExecutable: fixture.amq, SessionRoot: fixture.session, Handle: "codex", LaunchNonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	argv, identity := codexIdentitySlots([]string{fixture.provider, "-c", notify})
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "codex", LaunchNonce: nonce, Mode: AdapterModeCapture, Provider: CodexProvider, ProviderVersion: codexCaptureMinimumVersion,
		Backend: CommandsBackendName, Profile: CommandsProfile().Identity(), ProjectRoot: fixture.project, SessionRoot: fixture.session,
		Cwd: fixture.cwd, ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv: argv, DynamicArgv: identity, State: ExecutionPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("codex"); err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(fixture.root, lease, ConversationRecord{
		Version: ConversationVersion, Handle: "codex", State: CapturePending, ProviderVersion: codexCaptureMinimumVersion, LaunchNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	ticket, err = PrepareExecution(fixture.root, "codex", nonce, ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: fixture.provider, TargetArgv: ticket.TargetArgv,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

type executionFixture struct {
	project, session, cwd, provider, amq, injector string
	root                                           *fsq.DeliveryRoot
}

func newExecutionFixture(t *testing.T) executionFixture {
	t.Helper()
	project := t.TempDir()
	session := filepath.Join(project, "session")
	cwd := filepath.Join(project, "work")
	if err := os.MkdirAll(filepath.Join(session, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	provider := filepath.Join(binDir, "provider")
	amq := filepath.Join(binDir, "amq")
	injector := filepath.Join(binDir, "injector")
	for _, path := range []string{provider, amq, injector} {
		if err := os.WriteFile(path, []byte("executable"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := fsq.SnapshotDeliveryRoot(session)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(session, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return executionFixture{project: project, session: session, cwd: cwd, provider: provider, amq: amq, injector: injector, root: root}
}

func mustExecutionTicket(t *testing.T, fixture executionFixture, nonce string) ExecutionTicket {
	t.Helper()
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: nonce, Mode: AdapterModeMint,
		Provider: ClaudeProvider, ConversationID: nonce,
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv: []string{fixture.provider}, TargetEnv: map[string]string{"LANG": "C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func seedPendingCursorExecution(t *testing.T, fixture executionFixture, nonce string) (ExecutionTicket, ExecutionEnvelope) {
	t.Helper()
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("cursor"); err != nil {
		t.Fatal(err)
	}
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "cursor", LaunchNonce: nonce, Mode: AdapterModeCapture, Provider: CursorProvider,
		ProviderVersion: cursorCaptureVersion, PreSpawnAcquire: true,
		Backend: CommandsBackendName, Profile: CommandsProfile().Identity(),
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv:  []string{fixture.provider, "--resume", preSpawnConversationPlaceholder},
		DynamicArgv: []DynamicArg{{Index: 2, Kind: DynamicArgConversationID}}, TargetEnv: map[string]string{"LANG": "C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(fixture.root, lease, ConversationRecord{
		Version: ConversationVersion, Handle: "cursor", State: CapturePending,
		ProviderVersion: cursorCaptureVersion, LaunchNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	return ticket, ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: fixture.provider,
		TargetArgv: ticket.TargetArgv, Environment: []string{"LANG=C"},
	}
}

func TestExecutionTicketAllowsEmptyArgOnlyForDeclaredInitialInput(t *testing.T) {
	fixture := newExecutionFixture(t)
	base := ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: "12121212-1212-4212-8212-121212121212", Mode: AdapterModeMint,
		Provider: ClaudeProvider, ConversationID: "12121212-1212-4212-8212-121212121212",
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv: []string{fixture.provider, ""},
	}
	if _, err := NewExecutionTicket(base); err == nil || !strings.Contains(err.Error(), "target argv[1]") {
		t.Fatalf("undeclared empty argv error = %v", err)
	}
	base.InitialInput = &PlannedInitialInput{Kind: InitialInputArgument, SHA256: initialInputDigest(""), ArgvIndex: 1}
	if _, err := NewExecutionTicket(base); err != nil {
		t.Fatalf("declared empty initial argument rejected: %v", err)
	}
	base.InitialInput.ArgvIndex = 0
	if _, err := NewExecutionTicket(base); err == nil || !strings.Contains(err.Error(), "target argv[1]") {
		t.Fatalf("misdeclared empty argv error = %v", err)
	}
}

func TestValidateExecutionOptionsCanonicalDefaultsAndOmittedNonDefaults(t *testing.T) {
	tests := []struct {
		name      string
		execution *PrepareExecutionOptions
		wantError bool
	}{
		{name: "explicit defaults equal nil", execution: &PrepareExecutionOptions{WakeMode: "enabled"}},
		{name: "omitted require wake", execution: &PrepareExecutionOptions{WakeMode: "enabled", RequireWake: true}, wantError: true},
		{name: "omitted no gitignore", execution: &PrepareExecutionOptions{WakeMode: "enabled", NoGitignore: true}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExecutionFixture(t)
			nonce := "69696969-6969-4969-8969-696969696969"
			lease, err := AcquireLease(fixture.root, nonce)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.LockHandles("claude"); err != nil {
				t.Fatal(err)
			}
			ticket, err := NewExecutionTicket(ExecutionTicketRequest{
				Handle: "claude", LaunchNonce: nonce, Mode: AdapterModeCapture, Provider: CodexProvider,
				ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
				ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
				TargetArgv: []string{fixture.provider}, Execution: test.execution,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
				t.Fatal(err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
			err = ValidateExecutionOptions(fixture.root, "claude", nonce, nil)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "execution options changed") {
					t.Fatalf("omitted non-default error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonical default equality: %v", err)
			}
			loaded, err := LoadExecutionTicket(fixture.root, "claude")
			if err != nil {
				t.Fatal(err)
			}
			want := PrepareExecutionOptions{WakeMode: "enabled", InjectorMode: "none"}
			if loaded.Execution == nil || !reflect.DeepEqual(*loaded.Execution, want) {
				t.Fatalf("persisted canonical defaults = %#v, want %#v", loaded.Execution, want)
			}
		})
	}
}

// Pre-merge review found that preserving acquisition progress also prevented
// the proven-before-request failure path from returning its ticket to pending.
func TestCodexDaemonRetriesAfterCreationWasNotRequested(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "92929292-9292-4929-8929-929292929292"
	_, envelope := seedPendingCodexDaemonExecution(t, fixture, nonce)
	calls, registrations := 0, 0
	envelope.AcquireIdentity = func(ticket ExecutionTicket) (CaptureEvidence, error) {
		calls++
		if calls == 1 {
			return CaptureEvidence{}, ErrIdentityNotRequested
		}
		holder, err := HolderOf(os.Getpid())
		if err != nil {
			return CaptureEvidence{}, err
		}
		return ObserveCodexDaemonThread(ticket, testConversationID, filepath.Join(ticket.Cwd, "daemon.sock"), holder)
	}
	envelope.RecordIdentity = func(ExecutionTicket) error { registrations++; return nil }
	if _, err := PrepareExecution(fixture.root, "codex", nonce, envelope); !errors.Is(err, ErrIdentityNotRequested) {
		t.Fatalf("preflight error = %v", err)
	}
	prepared, err := PrepareExecution(fixture.root, "codex", nonce, envelope)
	if err != nil || prepared.State != ExecutionAcknowledged || calls != 2 || registrations != 1 {
		t.Fatalf("retry = %#v, %v; acquisition=%d registration=%d", prepared, err, calls, registrations)
	}
}

// Pro S1 finding 3: reconcile rollback deleted a pre-existing acquired ticket
// after a template mismatch or after preserving it and failing a sibling.
func TestReconcileRollbackPreservesAcquiredCodexTicket(t *testing.T) {
	for _, failure := range []string{"template_mismatch", "later_sibling"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newExecutionFixture(t)
			nonce := "94949494-9494-4949-8949-949494949494"
			ticket, envelope := seedPendingCodexDaemonExecution(t, fixture, nonce)
			envelope.RecordIdentity = func(ExecutionTicket) error { return nil }
			prepared, err := prepareExecution(fixture.root, ticket.Handle, nonce, envelope, &countingProviderAcquirer{id: testConversationID}, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan := AgentPlan{Handle: ticket.Handle, LaunchNonce: nonce, AdapterMode: ticket.Mode, Cwd: ticket.Cwd,
				ResumePolicy: ResumeEnabled, PreSpawnAcquire: true, Argv: ticket.TargetArgv, DynamicArgv: ticket.DynamicArgv}
			planned := []plannedAgent{{plan: plan, adapter: NewCodexAdapter(ticket.ProviderExecutable), providerVersion: CodexDaemonVersion}}
			if failure == "template_mismatch" {
				planned[0].plan.Argv = append(slices.Clone(plan.Argv), "changed prompt")
				planned[0].plan.InitialInput = &PlannedInitialInput{Kind: InitialInputArgument, SHA256: initialInputDigest("changed prompt"), ArgvIndex: 5}
			} else {
				planned = append(planned, plannedAgent{plan: AgentPlan{Handle: "other"}})
			}
			lease, err := AcquireLease(fixture.root, nonce)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lease.Release() }()
			if err := lease.LockHandles("codex", "other"); err != nil {
				t.Fatal(err)
			}
			err = writeExecutionTickets(ReconcileRequest{Root: fixture.root, ProjectRoot: fixture.project}, lease, planned, fixture.amq, CommandsBackendName, CommandsProfile().Identity())
			if err == nil {
				t.Fatal("expected failed replan")
			}
			after, err := LoadExecutionTicket(fixture.root, ticket.Handle)
			if err != nil || !reflect.DeepEqual(after, prepared) {
				t.Fatalf("rollback changed acquired ticket: %#v, %v", after, err)
			}
		})
	}
}

// Pro S1 finding 4: ready native conversations accepted intact capture evidence
// copied into another physical session root.
func TestCodexConversationRejectsCopiedSessionEvidence(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "95959595-9595-4959-8959-959595959595"
	ticket, envelope := seedPendingCodexDaemonExecution(t, fixture, nonce)
	envelope.RecordIdentity = func(ExecutionTicket) error { return nil }
	prepared, err := prepareExecution(fixture.root, ticket.Handle, nonce, envelope, &countingProviderAcquirer{id: testConversationID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	destination := newExecutionFixture(t)
	for _, source := range []string{ConversationPath(fixture.session, ticket.Handle), EvidencePath(fixture.session, prepared.EvidenceRefs[0])} {
		relative, err := filepath.Rel(fixture.session, source)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(destination.session, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadConversation(destination.root, ticket.Handle); err == nil || !strings.Contains(err.Error(), "physical session root") {
		t.Fatalf("foreign-root conversation = %v", err)
	}
}

// Pro S1 review R2: live executable checks in ticket decoding made completed
// native state unreadable after an upgrade and blocked authorized fresh plans.
func TestCompletedCodexTicketCanBeReplannedAfterExecutableReplacement(t *testing.T) {
	for _, replacement := range []string{"replace_file", "retarget_symlink"} {
		t.Run(replacement, func(t *testing.T) {
			fixture := newExecutionFixture(t)
			nonce := "99999999-9999-4999-8999-999999999999"
			ticket, envelope := seedPendingCodexDaemonExecution(t, fixture, nonce)
			envelope.RecordIdentity = func(ExecutionTicket) error { return nil }
			prepared, err := prepareExecution(fixture.root, ticket.Handle, nonce, envelope, &countingProviderAcquirer{id: testConversationID}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if replacement == "replace_file" {
				next := ticket.ProviderExecutable + "-replacement"
				if err := os.WriteFile(next, []byte("replacement executable"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(next, ticket.ProviderExecutable); err != nil {
					t.Fatal(err)
				}
			} else {
				next := ticket.ProviderExecutable + "-next"
				if err := os.WriteFile(next, []byte("replacement executable"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(ticket.ProviderExecutable); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(next, ticket.ProviderExecutable); err != nil {
					t.Fatal(err)
				}
			}
			historical, err := LoadExecutionTicket(fixture.root, ticket.Handle)
			if err != nil || !reflect.DeepEqual(historical, prepared) {
				t.Fatalf("historical ticket unreadable: %#v, %v", historical, err)
			}
			if _, err := PrepareExecution(fixture.root, ticket.Handle, nonce, envelope); err == nil || !strings.Contains(err.Error(), "identity changed") {
				t.Fatalf("old execution was not refused at its live boundary: %v", err)
			}
			nextNonce := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			lease, err := AcquireLease(fixture.root, nextNonce)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lease.Release() }()
			if err := lease.LockHandles(ticket.Handle); err != nil {
				t.Fatal(err)
			}
			planned := []plannedAgent{{adapter: NewCodexAdapter(ticket.TargetArgv[0]), providerVersion: CodexDaemonVersion,
				plan: AgentPlan{Handle: ticket.Handle, LaunchNonce: nextNonce, AdapterMode: ticket.Mode, Cwd: ticket.Cwd,
					ResumePolicy: ResumeEnabled, PreSpawnAcquire: true, Argv: ticket.TargetArgv, DynamicArgv: ticket.DynamicArgv}}}
			if err := writeExecutionTickets(ReconcileRequest{Root: fixture.root, ProjectRoot: fixture.project}, lease, planned, fixture.amq, CommandsBackendName, CommandsProfile().Identity()); err != nil {
				t.Fatal(err)
			}
			current, err := LoadExecutionTicket(fixture.root, ticket.Handle)
			if err != nil || current.LaunchNonce != nextNonce || current.State != ExecutionPending || current.ProviderExecutableIdentity == historical.ProviderExecutableIdentity {
				t.Fatalf("new generation = %#v, %v", current, err)
			}
		})
	}
}

// Claude S1 F2: explicit recovery must abandon only the exact uncertain
// ticket the operator inspected, while preserving concurrent progress.
func TestUncertainExecutionCleanupPreservesChangedTicket(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "acacacac-acac-4cac-8cac-acacacacacac"
	_, envelope := seedPendingCodexDaemonExecution(t, fixture, nonce)
	envelope.AcquireIdentity = func(ExecutionTicket) (CaptureEvidence, error) {
		return CaptureEvidence{}, errors.New("lost thread/start reply")
	}
	if _, err := PrepareExecution(fixture.root, "codex", nonce, envelope); !errors.Is(err, ErrCodexAcquisitionUncertain) {
		t.Fatalf("uncertain acquisition = %v", err)
	}
	target, err := InspectExecutionCleanup(fixture.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if err := lease.LockHandles("codex"); err != nil {
		t.Fatal(err)
	}
	changed := target.Ticket
	changed.Reason = "new operator observation"
	if err := WriteExecutionTicket(fixture.root, lease, changed); err != nil {
		t.Fatal(err)
	}
	if err := ClearExecutionForCleanup(fixture.root, lease, target); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed cleanup = %v", err)
	}
	current, err := InspectExecutionCleanup(fixture.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := ClearExecutionForCleanup(fixture.root, lease, current); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExecutionTicket(fixture.root, "codex"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit cleanup retained ticket: %v", err)
	}
}
