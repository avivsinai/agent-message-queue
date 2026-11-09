package launch

import (
	"errors"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// CodexReviewerLine says that Codex's automatic approvals reviewer answers a
// seat's approvals, so Buzz gets none. The reviewer is the owner's choice.
func CodexReviewerLine(reviewer string) string {
	return "Buzz gets no Codex approvals: Codex's automatic reviewer answers them (approvals_reviewer = " + reviewer + ")"
}

// CodexTerminalOnlyReason reads the same durable explanation for launch,
// remote status, and operational diagnosis. It does not create a root.
func CodexTerminalOnlyReason(path, handle string) (string, error) {
	identity, err := fsq.SnapshotDeliveryRoot(path)
	if err != nil {
		return "", err
	}
	root, err := fsq.OpenDeliveryRoot(path, identity)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	ticket, err := LoadExecutionTicket(root, handle)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if ticket.Provider != CodexProvider || ticket.PreSpawnAcquire {
		return "", nil
	}
	if ticket.TerminalOnlyReason != "" {
		return ticket.TerminalOnlyReason, nil
	}
	return "this managed Codex seat uses an embedded app-server", nil
}
