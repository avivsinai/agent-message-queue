package launch

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// Claude S1 R-1, acquired_wedge_test.go: after a daemon restart or executable
// upgrade, retry and replacement both refused, and cleanup offered no exit.
func TestAcquiredExecutionCleanupAllowsAReplacementGeneration(t *testing.T) {
	for _, stage := range []string{"evidence_persisted", string(ExecutionIdentityAcquired), string(ExecutionSpawnAttempted)} {
		state := ExecutionState(stage)
		if stage == "evidence_persisted" {
			state = ExecutionIdentityRequested
		}
		t.Run(stage, func(t *testing.T) {
			fixture := newExecutionFixture(t)
			nonce := "92929292-9292-4929-8929-929292929292"
			ticket, envelope := seedPendingCodexDaemonExecution(t, fixture, nonce)
			replaced := errors.New("codex daemon endpoint changed since thread acquisition")
			envelope.RecordIdentity = func(ExecutionTicket) error { return nil }
			acquirer := &countingProviderAcquirer{id: testConversationID}
			_, err := prepareExecution(fixture.root, "codex", nonce, envelope, acquirer, func(at string) error {
				if at == stage {
					return replaced
				}
				return nil
			})
			if !errors.Is(err, replaced) {
				t.Fatal(err)
			}
			envelope.RecordIdentity = func(ExecutionTicket) error { return replaced }
			if state == ExecutionSpawnAttempted || state == ExecutionIdentityRequested {
				next := ticket.ProviderExecutable + "-replacement"
				if err := os.WriteFile(next, []byte("replacement"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(next, ticket.ProviderExecutable); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := PrepareExecution(fixture.root, "codex", nonce, envelope); err == nil {
				t.Fatal("expected retry to refuse changed provider")
			}
			stored, err := LoadExecutionTicket(fixture.root, "codex")
			if err != nil || stored.State != state {
				t.Fatalf("preserved ticket=%#v, %v", stored, err)
			}
			before, err := LoadConversation(fixture.root, "codex")
			if err != nil {
				t.Fatal(err)
			}
			target, err := InspectExecutionCleanup(fixture.root, "codex")
			if err != nil {
				t.Fatalf("no explicit cleanup exit for %s: %v", state, err)
			}
			if target.ThreadID != testConversationID {
				t.Fatalf("cleanup lost the known thread: %#v", target.Ticket)
			}
			lease, err := AcquireLease(fixture.root, nonce)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.LockHandles("codex"); err != nil {
				t.Fatal(err)
			}
			if err := ClearExecutionForCleanup(fixture.root, lease, target); err != nil {
				t.Fatal(err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
			after, err := LoadConversation(fixture.root, "codex")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("cleanup changed conversation: %#v, %v", after, err)
			}
			if _, _, err := ReadEvidence(fixture.root, target.evidenceRefs[0]); err != nil {
				t.Fatalf("cleanup removed known-thread evidence: %v", err)
			}
			nextNonce := "93939393-9393-4939-8939-939393939393"
			lease, err = AcquireLease(fixture.root, nextNonce)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lease.Release() }()
			if err := lease.LockHandles("codex"); err != nil {
				t.Fatal(err)
			}
			plan := AgentPlan{Handle: "codex", LaunchNonce: nextNonce, AdapterMode: ticket.Mode, Cwd: ticket.Cwd,
				ResumePolicy: ResumeEnabled, PreSpawnAcquire: true, Argv: ticket.TargetArgv, DynamicArgv: ticket.DynamicArgv}
			planned := []plannedAgent{{plan: plan, adapter: NewCodexAdapter(ticket.TargetArgv[0]), providerVersion: CodexDaemonVersion}}
			if err := writeExecutionTickets(ReconcileRequest{Root: fixture.root, ProjectRoot: fixture.project}, lease, planned, fixture.amq, CommandsBackendName, CommandsProfile().Identity()); err != nil {
				t.Fatalf("later generation remains wedged: %v", err)
			}
		})
	}
}
