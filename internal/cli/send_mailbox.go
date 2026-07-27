package cli

import (
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func prepareLocalSendMailboxes(root *fsq.DeliveryRoot, authorization *fsq.MailboxConfigAuthorization, displayRoot string, recipients []string) error {
	result := fsq.RepairMailboxLayoutForAgentsWithAuthorization(root, authorization, recipients)
	if result.Status != "repaired" {
		return sendMailboxRepairError(displayRoot, result)
	}
	return nil
}

func sendMailboxAuthorizationError(displayRoot string, inventory fsq.MailboxInventory) error {
	if inventory.ActiveConfigStatus == string(fsq.MailboxPathMissing) {
		return NotFoundError(
			"AMQ root %s is not initialized (%s); run 'amq init --root %s --agents <agents>' or 'amq coop init --root %s'",
			displayRoot,
			inventory.ActiveConfigIssue,
			displayRoot,
			displayRoot,
		)
	}
	return fmt.Errorf(
		"cannot authorize destination mailbox layout at %s: active config %s (%s)",
		displayRoot,
		inventory.ActiveConfigStatus,
		inventory.ActiveConfigIssue,
	)
}

func sendMailboxRepairError(displayRoot string, result fsq.MailboxRepairResult) error {
	if result.Failure == nil {
		return fmt.Errorf("destination mailbox repair at %s ended with status %s", displayRoot, result.Status)
	}
	return fmt.Errorf(
		"destination mailbox repair at %s failed (%s/%s%s): %s",
		displayRoot,
		result.Failure.Code,
		result.Failure.Stage,
		sendMailboxRepairPath(result.Failure.Path),
		result.Failure.Message,
	)
}

func sendMailboxRepairPath(path string) string {
	if path == "" {
		return ""
	}
	return " " + path
}
