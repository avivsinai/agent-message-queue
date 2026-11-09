package launch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

var ErrCodexAcquisitionUncertain = errors.New("codex thread acquisition is uncertain")

var ErrCodexExecutionRecoveryRequired = errors.New("codex native execution requires recovery")

func codexExecutionRecoveryRequired(ticket ExecutionTicket) error {
	if ticket.State == ExecutionIdentityRequested {
		return uncertainCodexAcquisition(ticket)
	}
	return fmt.Errorf("%w for %s (nonce %s, thread %s); retry the original emitted command to reuse the acquired identity; if the daemon or executable changed, inspect amq cleanup --launch-execution %s --root %s --dry-run to explicitly abandon it", ErrCodexExecutionRecoveryRequired, ticket.Handle, ticket.LaunchNonce, ticket.ConversationID, ticket.Handle, shellQuote(ticket.SessionRoot))
}

func uncertainCodexAcquisition(ticket ExecutionTicket) error {
	return fmt.Errorf("%w for %s; inspect the daemon for an orphan thread, then inspect amq cleanup --launch-execution %s --root %s --dry-run (use --yes to abandon this uncertain attempt)",
		ErrCodexAcquisitionUncertain, ticket.Handle, ticket.Handle, shellQuote(ticket.SessionRoot))
}

// ExecutionCleanupTarget captures the exact incomplete native ticket shown to the
// operator. Its private bytes and file identity cannot be reconstructed by a caller.
type ExecutionCleanupTarget struct {
	Ticket       ExecutionTicket
	ThreadID     string
	evidenceRefs []string
	raw          []byte
	info         os.FileInfo
}

func InspectExecutionCleanup(root *fsq.DeliveryRoot, handle string) (ExecutionCleanupTarget, error) {
	ticket, raw, info, err := readExecutionTicket(root, handle)
	if err != nil {
		return ExecutionCleanupTarget{}, err
	}
	if err := root.VerifyBase(); err != nil {
		return ExecutionCleanupTarget{}, err
	}
	_, sessionIdentity, err := canonicalDirectory(root.Base())
	if err != nil || !fsq.SamePersistedTreeIdentity(ticket.SessionIdentity, sessionIdentity) {
		return ExecutionCleanupTarget{}, fmt.Errorf("execution ticket belongs to another physical session root")
	}
	if ticket.Provider != CodexProvider || !ticket.PreSpawnAcquire {
		return ExecutionCleanupTarget{}, fmt.Errorf("cleanup requires an incomplete native Codex execution ticket")
	}
	threadID := ticket.ConversationID
	refs := slices.Clone(ticket.EvidenceRefs)
	switch ticket.State {
	case ExecutionIdentityRequested:
		evidence, ref, found, err := findProviderCaptureEvidence(root, CodexProvider, handle, ticket.LaunchNonce, ticket.ProviderVersion)
		if err != nil {
			return ExecutionCleanupTarget{}, err
		}
		if found {
			// A crash may leave capture durable before the ticket's identity update.
			// Validate and display that known thread without needing the old provider.
			bound := ticket
			bound.ConversationID, bound.EvidenceRefs = evidence.conversationID, []string{ref}
			if err := validateCodexDaemonExecutionEvidence(root, bound); err != nil {
				return ExecutionCleanupTarget{}, err
			}
			threadID, refs = bound.ConversationID, bound.EvidenceRefs
		}
	case ExecutionIdentityAcquired, ExecutionSpawnAttempted:
		if err := validateCodexDaemonExecutionEvidence(root, ticket); err != nil {
			return ExecutionCleanupTarget{}, err
		}
	default:
		return ExecutionCleanupTarget{}, fmt.Errorf("cleanup requires identity_requested, identity_acquired, or spawn_attempted state")
	}
	return ExecutionCleanupTarget{Ticket: ticket, ThreadID: threadID, evidenceRefs: refs, raw: raw, info: info}, nil
}

// ClearExecutionForCleanup abandons only an unchanged incomplete native execution
// under its original session lease and handle lock. It never touches Codex.
func ClearExecutionForCleanup(root *fsq.DeliveryRoot, lease *Lease, expected ExecutionCleanupTarget) error {
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	if !lease.holdsHandle(expected.Ticket.Handle) || lease.LaunchNonce() != expected.Ticket.LaunchNonce {
		return fmt.Errorf("execution cleanup does not hold the exact launch generation and handle")
	}
	current, err := InspectExecutionCleanup(root, expected.Ticket.Handle)
	if err != nil {
		return err
	}
	if current.Ticket.LaunchNonce != lease.LaunchNonce() || expected.info == nil || !os.SameFile(current.info, expected.info) || !bytes.Equal(current.raw, expected.raw) || !slices.Equal(current.evidenceRefs, expected.evidenceRefs) {
		return fmt.Errorf("execution ticket changed before cleanup; inspect it again")
	}
	return root.Remove(filepath.Join(executionDirectory, expected.Ticket.Handle+".json"))
}
