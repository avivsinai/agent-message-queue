package cli

import (
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func prepareLocalSendMailboxes(root *fsq.DeliveryRoot, displayRoot string, recipients []string) error {
	inventory, err := fsq.InspectMailboxLayout(root)
	if err != nil {
		return fmt.Errorf("inspect destination mailbox layout: %w", err)
	}
	if inventory.ActiveConfigStatus != "ok" {
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
			"cannot prepare destination mailbox layout at %s: active config %s (%s)",
			displayRoot,
			inventory.ActiveConfigStatus,
			inventory.ActiveConfigIssue,
		)
	}

	configured := make(map[string]bool, len(inventory.ConfiguredAgents))
	for _, handle := range inventory.ConfiguredAgents {
		configured[handle] = true
	}
	status := make(map[string]string, len(inventory.Mailboxes))
	for _, mailbox := range inventory.Mailboxes {
		status[mailbox.Handle] = mailbox.Status
	}

	repairConfigured := false
	additional := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		if configured[recipient] {
			if status[recipient] != "ok" {
				repairConfigured = true
			}
			continue
		}
		additional = append(additional, recipient)
	}

	if repairConfigured {
		if result := fsq.RepairMailboxLayout(root); result.Status != "repaired" {
			return sendMailboxRepairError(displayRoot, result)
		}
	}
	if len(additional) > 0 {
		if result := fsq.RepairMailboxLayoutForAgents(root, additional); result.Status != "repaired" {
			return sendMailboxRepairError(displayRoot, result)
		}
	}

	verified, err := fsq.InspectMailboxLayout(root)
	if err != nil {
		return fmt.Errorf("verify destination mailbox layout: %w", err)
	}
	verifiedStatus := make(map[string]string, len(verified.Mailboxes))
	for _, mailbox := range verified.Mailboxes {
		verifiedStatus[mailbox.Handle] = mailbox.Status
	}
	for _, recipient := range recipients {
		if verifiedStatus[recipient] != "ok" {
			return fmt.Errorf("destination mailbox for %q is incomplete at root %s", recipient, displayRoot)
		}
	}
	return nil
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
