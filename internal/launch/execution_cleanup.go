package launch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

var ErrCodexAcquisitionUncertain = errors.New("codex thread acquisition is uncertain")

var ErrCodexExecutionRecoveryRequired = errors.New("codex native execution requires recovery")

func codexExecutionRecoveryRequired(ticket ExecutionTicket) error {
	if ticket.State == ExecutionIdentityRequested {
		return uncertainCodexAcquisition(ticket)
	}
	return fmt.Errorf("%w for %s (nonce %s, thread %s); retry the original emitted command to reuse the acquired identity", ErrCodexExecutionRecoveryRequired, ticket.Handle, ticket.LaunchNonce, ticket.ConversationID)
}

func uncertainCodexAcquisition(ticket ExecutionTicket) error {
	return fmt.Errorf("%w for %s; inspect the daemon for an orphan thread, then inspect amq cleanup --launch-execution %s --root %s --dry-run (use --yes to abandon this uncertain attempt)",
		ErrCodexAcquisitionUncertain, ticket.Handle, ticket.Handle, shellQuote(ticket.SessionRoot))
}

// ExecutionCleanupTarget captures the exact uncertain ticket shown to the
// operator. Its private bytes and file identity cannot be reconstructed by a caller.
type ExecutionCleanupTarget struct {
	Ticket ExecutionTicket
	raw    []byte
	info   os.FileInfo
}

func InspectUncertainExecution(root *fsq.DeliveryRoot, handle string) (ExecutionCleanupTarget, error) {
	ticket, raw, info, err := readExecutionTicket(root, handle)
	if err != nil {
		return ExecutionCleanupTarget{}, err
	}
	if err := root.VerifyBase(); err != nil {
		return ExecutionCleanupTarget{}, err
	}
	_, sessionIdentity, err := canonicalDirectory(root.Base())
	if err != nil || sessionIdentity != ticket.SessionIdentity {
		return ExecutionCleanupTarget{}, fmt.Errorf("execution ticket belongs to another physical session root")
	}
	if ticket.Provider != CodexProvider || !ticket.PreSpawnAcquire || ticket.State != ExecutionIdentityRequested {
		return ExecutionCleanupTarget{}, fmt.Errorf("cleanup requires an identity_requested native Codex ticket")
	}
	// Persisted evidence proves more progress than this state alone records.
	// Such a ticket must be recovered rather than abandoned as an unknown request.
	_, _, found, err := findProviderCaptureEvidence(root, CodexProvider, handle, ticket.LaunchNonce, ticket.ProviderVersion)
	if err != nil {
		return ExecutionCleanupTarget{}, err
	}
	if found {
		return ExecutionCleanupTarget{}, fmt.Errorf("native acquisition evidence exists; retry execution to recover its thread")
	}
	return ExecutionCleanupTarget{Ticket: ticket, raw: raw, info: info}, nil
}

// ClearUncertainExecution abandons only an unchanged, evidence-free request
// under its original session lease and handle lock. It never touches Codex.
func ClearUncertainExecution(root *fsq.DeliveryRoot, lease *Lease, expected ExecutionCleanupTarget) error {
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	if !lease.holdsHandle(expected.Ticket.Handle) || lease.LaunchNonce() != expected.Ticket.LaunchNonce {
		return fmt.Errorf("execution cleanup does not hold the exact launch generation and handle")
	}
	current, err := InspectUncertainExecution(root, expected.Ticket.Handle)
	if err != nil {
		return err
	}
	if current.Ticket.LaunchNonce != lease.LaunchNonce() || expected.info == nil || !os.SameFile(current.info, expected.info) || !bytes.Equal(current.raw, expected.raw) {
		return fmt.Errorf("execution ticket changed before cleanup; inspect it again")
	}
	return root.Remove(filepath.Join(executionDirectory, expected.Ticket.Handle+".json"))
}
