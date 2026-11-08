package cli

import (
	"os"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func cleanupLaunchExecution(rootPath, handle string, jsonOutput, dryRun, yes, ignorePin bool) error {
	if err := guardMailboxContext("cleanup --launch-execution", rootPath, false, ignorePin, true); err != nil {
		return err
	}
	identity, err := snapshotMailboxDeliveryRoot(rootPath, false, ignorePin)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	target, err := launch.InspectExecutionCleanup(root, handle)
	if err != nil {
		return ActionRequiredError("inspect incomplete native execution: %v", err)
	}
	report := map[string]any{"path": launch.ExecutionTicketPath(root.Base(), handle), "handle": handle,
		"launch_nonce": target.Ticket.LaunchNonce, "state": target.Ticket.State, "thread_id": target.ThreadID,
		"effect": "abandon incomplete native execution; Codex threads, evidence, routing, and journal remain untouched"}
	output := func(removed bool) error {
		if jsonOutput {
			return writeJSON(os.Stdout, map[string]any{"removed": removed, "launch_execution": report})
		}
		action := "Would remove"
		if removed {
			action = "Removed"
		}
		thread := target.ThreadID
		if thread == "" {
			thread = "unknown"
		}
		return writeStdout("%s native execution ticket %s (nonce %s, state %s, thread %s). Codex threads, evidence, routing, and journal remain untouched.\n", action, report["path"], target.Ticket.LaunchNonce, target.Ticket.State, thread)
	}
	if dryRun {
		return output(false)
	}
	if !yes {
		if err := output(false); err != nil {
			return err
		}
		confirmed, err := confirmPrompt("Abandon this incomplete native execution and leave its Codex thread untouched?")
		if err != nil {
			return err
		}
		if !confirmed {
			return writeStdoutLine("Aborted.")
		}
	}
	if err := guardMailboxContext("cleanup --launch-execution", rootPath, false, ignorePin, true); err != nil {
		return err
	}
	lease, err := launch.AcquireLease(root, target.Ticket.LaunchNonce)
	if err != nil {
		return ActionRequiredError("execution cleanup cannot acquire the session lease: %v", err)
	}
	defer func() { _ = lease.Release() }()
	if err := lease.LockHandles(handle); err != nil {
		return ActionRequiredError("execution cleanup cannot lock %s: %v", handle, err)
	}
	if err := launch.ClearExecutionForCleanup(root, lease, target); err != nil {
		return ActionRequiredError("preserve native execution: %v", err)
	}
	return output(true)
}
