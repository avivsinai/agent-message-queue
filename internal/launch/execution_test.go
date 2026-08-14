package launch

import (
	"os"
	"path/filepath"
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
	ticket, err := NewExecutionTicket(ExecutionTicketRequest{
		Handle: "claude", LaunchNonce: lease.LaunchNonce(), Mode: AdapterModeMint,
		Provider: ClaudeProvider, ConversationID: lease.LaunchNonce(),
		ProjectRoot: fixture.project, SessionRoot: fixture.session, Cwd: fixture.cwd,
		ProviderExecutable: fixture.provider, AMQExecutable: fixture.amq,
		TargetArgv: []string{fixture.provider, "--session-id", lease.LaunchNonce()},
		TargetEnv:  map[string]string{"LANG": "C", "NO_COLOR": "1"},
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
	if loaded.State != ExecutionPending || loaded.TargetEnv["LANG"] != "C" || loaded.EnvDigest == "" {
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

type executionFixture struct {
	project, session, cwd, provider, amq string
	root                                 *fsq.DeliveryRoot
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
	for _, path := range []string{provider, amq} {
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
	return executionFixture{project: project, session: session, cwd: cwd, provider: provider, amq: amq, root: root}
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
