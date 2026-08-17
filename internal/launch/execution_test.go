package launch

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: lease.LaunchNonce(), Mode: AdapterModeMint,
		Provider: ClaudeProvider, ConversationID: lease.LaunchNonce(),
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv: []string{fixture.provider, "--session-id", lease.LaunchNonce()},
		TargetEnv:  map[string]string{"LANG": "C", "NO_COLOR": "1"},
		Execution:  execution,
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
	if loaded.State != ExecutionPending || loaded.TargetEnv["LANG"] != "C" || loaded.EnvDigest == "" || !reflect.DeepEqual(loaded.Execution, execution) {
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

func TestExecutionTicketWritesRequireNonceAndHandleLock(t *testing.T) {
	fixture := newExecutionFixture(t)
	lease, err := AcquireLease(fixture.root, "22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	ticket := mustExecutionTicket(t, fixture, "33333333-3333-4333-8333-333333333333")
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("wrong nonce write error = %v", err)
	}
	ticket = mustExecutionTicket(t, fixture, lease.LaunchNonce())
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err == nil || !strings.Contains(err.Error(), "hold handle") {
		t.Fatalf("unlocked handle write error = %v", err)
	}
}

func TestExecutionTicketLoadRejectsUnknownFieldsAndPermissiveMode(t *testing.T) {
	fixture := newExecutionFixture(t)
	lease, err := AcquireLease(fixture.root, "44444444-4444-4444-8444-444444444444")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	ticket := mustExecutionTicket(t, fixture, lease.LaunchNonce())
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	path := ExecutionTicketPath(fixture.session, "claude")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-2], []byte(",\"extra\":true}\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExecutionTicket(fixture.root, "claude"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field load error = %v", err)
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

func TestPrepareExecutionWaitsForOwnLauncherLease(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "57575757-5757-4757-8757-575757575757"
	ticket, envelope := seedPendingMintExecution(t, fixture, nonce)
	blocker, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		ticket ExecutionTicket
		err    error
	}
	done := make(chan result, 1)
	go func() {
		prepared, prepareErr := PrepareExecution(fixture.root, "claude", nonce, envelope)
		done <- result{ticket: prepared, err: prepareErr}
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case early := <-done:
		t.Fatalf("prepare returned before own launcher released its lease: %#v, %v", early.ticket, early.err)
	default:
	}
	if err := blocker.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.ticket.State != ExecutionAcknowledged || got.ticket.LaunchNonce != ticket.LaunchNonce {
			t.Fatalf("prepare after own lease release = %#v, %v", got.ticket, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prepare did not acquire the released own-launcher lease")
	}
}

func TestPrepareExecutionRefusesDifferentLauncherLeaseImmediately(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "58585858-5858-4858-8858-585858585858"
	_, envelope := seedPendingMintExecution(t, fixture, nonce)
	blocker, err := AcquireLease(fixture.root, "59595959-5959-4959-8959-595959595959")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Release() }()
	started := time.Now()
	_, err = PrepareExecution(fixture.root, "claude", nonce, envelope)
	var held *LeaseHeldError
	if !errors.As(err, &held) || held.Nonce == nonce {
		t.Fatalf("different-launcher lease error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("different-launcher refusal took %s, want immediate", elapsed)
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

func TestPrepareExecutionRejectsRetargetedInjectorWithoutPromotion(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "67676767-6767-4767-8767-676767676767"
	injectorDir := t.TempDir()
	first := filepath.Join(injectorDir, "injector-v1")
	second := filepath.Join(injectorDir, "injector-v2")
	link := filepath.Join(injectorDir, "injector")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	execution := &PrepareExecutionOptions{
		WakeMode: "enabled", InjectorMode: "raw", InjectorVia: link,
		InjectorArgs: []string{"--fixed", "ordered"},
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
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv: []string{fixture.provider}, TargetEnv: map[string]string{"LANG": "C"}, Execution: execution,
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
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecutionOptions(fixture.root, "claude", nonce, execution); err == nil || !strings.Contains(err.Error(), "injector executable identity changed") {
		t.Fatalf("pre-wake retarget error = %v", err)
	}
	_, err = PrepareExecution(fixture.root, "claude", nonce, ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: fixture.provider,
		TargetArgv: ticket.TargetArgv, Environment: []string{"LANG=C"}, Execution: execution,
	})
	if err == nil || !strings.Contains(err.Error(), "injector executable identity changed") {
		t.Fatalf("retarget error = %v", err)
	}
	record, loadErr := LoadConversation(fixture.root, "claude")
	if loadErr != nil || record.State != CapturePending {
		t.Fatalf("conversation mutated = %#v, %v", record, loadErr)
	}
}

func TestPrepareExecutionRejectsChangedInjectorArgs(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "68686868-6868-4868-8868-686868686868"
	execution := &PrepareExecutionOptions{
		WakeMode: "enabled", InjectorMode: "raw", InjectorVia: fixture.injector,
		InjectorArgs: []string{"--fixed", "ordered"},
	}
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
		TargetArgv: []string{fixture.provider}, TargetEnv: map[string]string{"LANG": "C"}, Execution: execution,
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
	changed := clonePrepareExecutionOptions(execution)
	changed.InjectorArgs = []string{"ordered", "--fixed"}
	if err := ValidateExecutionOptions(fixture.root, "claude", nonce, changed); err == nil || !strings.Contains(err.Error(), "execution options changed") {
		t.Fatalf("pre-wake reordered args error = %v", err)
	}
	_, err = PrepareExecution(fixture.root, "claude", nonce, ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: fixture.provider,
		TargetArgv: ticket.TargetArgv, Environment: []string{"LANG=C"}, Execution: changed,
	})
	if err == nil || !strings.Contains(err.Error(), "execution options changed") {
		t.Fatalf("reordered args error = %v", err)
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

type countingCursorAcquirer struct {
	calls int
	id    string
}

func (acquirer *countingCursorAcquirer) Acquire(ticket ExecutionTicket) (CaptureEvidence, error) {
	acquirer.calls++
	return ParseCursorCreateChatEvidence([]byte(acquirer.id+"\n"), ticket.LaunchNonce, ticket.Handle, ticket.ProviderVersion)
}

func TestPrepareCursorPreSpawnReusesAcquisitionAcrossCrashes(t *testing.T) {
	for _, crashStage := range []string{"evidence_persisted", "identity_acquired", "spawn_attempted"} {
		t.Run(crashStage, func(t *testing.T) {
			fixture := newExecutionFixture(t)
			nonce := "91919191-9191-4919-8919-919191919191"
			ticket, envelope := seedPendingCursorExecution(t, fixture, nonce)
			acquirer := &countingCursorAcquirer{id: testConversationID}
			crash := errors.New("simulated crash")
			_, err := prepareExecution(fixture.root, "cursor", nonce, envelope, acquirer, func(stage string) error {
				if stage == crashStage {
					return crash
				}
				return nil
			})
			if !errors.Is(err, crash) {
				t.Fatalf("first prepare error = %v", err)
			}
			prepared, err := prepareExecution(fixture.root, "cursor", nonce, envelope, acquirer, nil)
			if err != nil || prepared.State != ExecutionAcknowledged || acquirer.calls != 1 {
				t.Fatalf("retry prepare = %#v, %v; acquisition calls = %d", prepared, err, acquirer.calls)
			}
			argv, err := ResolveExecutionArgv(prepared)
			if err != nil || argv[ticket.DynamicArgv[0].Index] != testConversationID || argv[ticket.DynamicArgv[0].Index] == preSpawnConversationPlaceholder {
				t.Fatalf("resolved argv = %#v, %v", argv, err)
			}
			record, err := LoadConversation(fixture.root, "cursor")
			if err != nil || record.State != CaptureReady || record.Identity.Provider != CursorProvider ||
				record.Identity.ID != testConversationID || len(record.EvidenceRefs) != 1 {
				t.Fatalf("ready Cursor conversation = %#v, %v", record, err)
			}
		})
	}
}

func TestRevertCursorExecutionRetainsAcquiredIdentity(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "92929292-9292-4929-8929-929292929292"
	_, envelope := seedPendingCursorExecution(t, fixture, nonce)
	acquirer := &countingCursorAcquirer{id: testConversationID}
	prepared, err := prepareExecution(fixture.root, "cursor", nonce, envelope, acquirer, nil)
	if err != nil || prepared.State != ExecutionAcknowledged {
		t.Fatalf("prepare = %#v, %v", prepared, err)
	}
	if err := RevertExecution(fixture.root, "cursor", nonce); err != nil {
		t.Fatal(err)
	}
	reverted, err := LoadExecutionTicket(fixture.root, "cursor")
	if err != nil || reverted.State != ExecutionIdentityAcquired || reverted.ConversationID != testConversationID || len(reverted.EvidenceRefs) != 1 {
		t.Fatalf("reverted Cursor ticket = %#v, %v", reverted, err)
	}
	if _, err := prepareExecution(fixture.root, "cursor", nonce, envelope, acquirer, nil); err != nil || acquirer.calls != 1 {
		t.Fatalf("retry after revert = %v; acquisition calls = %d", err, acquirer.calls)
	}
}

func TestPrepareCursorFailsClosedOnTamperedAcquisitionEvidence(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "93939393-9393-4939-8939-939393939393"
	_, envelope := seedPendingCursorExecution(t, fixture, nonce)
	acquirer := &countingCursorAcquirer{id: testConversationID}
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

func TestPrepareCursorRejectsValidEvidenceBoundToAnotherNonce(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "95959595-9595-4959-8959-959595959595"
	_, envelope := seedPendingCursorExecution(t, fixture, nonce)
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("cursor"); err != nil {
		t.Fatal(err)
	}
	foreignNonce := "96969696-9696-4969-8969-969696969696"
	foreign, err := ParseCursorCreateChatEvidence([]byte(testConversationID), foreignNonce, "cursor", cursorCaptureVersion)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := persistProviderCaptureEvidence(fixture.root, lease, "cursor", []CaptureEvidence{foreign})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := LoadExecutionTicket(fixture.root, "cursor")
	if err != nil {
		t.Fatal(err)
	}
	ticket.State, ticket.Reason = ExecutionIdentityAcquired, "identity_acquired"
	ticket.ConversationID, ticket.EvidenceRefs = testConversationID, refs
	if err := WriteExecutionTicket(fixture.root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	acquirer := &countingCursorAcquirer{id: testConversationID}
	if _, err := prepareExecution(fixture.root, "cursor", nonce, envelope, acquirer, nil); err == nil || !strings.Contains(err.Error(), "cursor execution evidence binding mismatch") {
		t.Fatalf("foreign evidence error = %v", err)
	}
	loaded, err := LoadExecutionTicket(fixture.root, "cursor")
	if err != nil || loaded.State != ExecutionIdentityAcquired || acquirer.calls != 0 {
		t.Fatalf("ticket after foreign evidence refusal = %#v, %v; acquisition calls = %d", loaded, err, acquirer.calls)
	}
}

func TestCursorOrphanEvidenceRejectsSameIdentityWithDifferentBytes(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "94949494-9494-4949-8949-949494949494"
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if err := lease.LockHandles("cursor"); err != nil {
		t.Fatal(err)
	}
	first, err := ParseCursorCreateChatEvidence([]byte(testConversationID), nonce, "cursor", cursorCaptureVersion)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseCursorCreateChatEvidence([]byte(testConversationID+"\n"), nonce, "cursor", cursorCaptureVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistProviderCaptureEvidence(fixture.root, lease, "cursor", []CaptureEvidence{first, second}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := findCursorCaptureEvidence(fixture.root, "cursor", nonce, cursorCaptureVersion); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous orphan evidence error = %v", err)
	}
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

func seedPendingMintExecution(t *testing.T, fixture executionFixture, nonce string) (ExecutionTicket, ExecutionEnvelope) {
	t.Helper()
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
	return ticket, ExecutionEnvelope{
		Cwd: fixture.cwd, AMQExecutable: fixture.amq, ProviderExecutable: fixture.provider,
		TargetArgv: ticket.TargetArgv, Environment: []string{"LANG=C"},
	}
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
