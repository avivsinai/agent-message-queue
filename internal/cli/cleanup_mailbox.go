package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// countWaitingMail counts non-dot regular files in one waiting leaf through the
// root capability. A missing directory counts as empty: a partial mailbox is
// exactly the state this command exists to retire.
func countWaitingMail(root *fsq.DeliveryRoot, rel string) (int, error) {
	entries, err := root.ReadDir(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		count++
	}
	return count, nil
}

// mailboxRemoveAgentLocked removes handle from the agents list in
// meta/config.json under the config lock, preserving every other key
// (MarshalPreservingUnknowns). A config without the handle is a no-op.
func mailboxRemoveAgentLocked(root *fsq.DeliveryRoot, handle string) (bool, error) {
	data, err := root.ReadFile("meta/config.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read config: %w", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, fmt.Errorf("parse config: %w", err)
	}
	kept := cfg.Agents[:0:0]
	found := false
	for _, agent := range cfg.Agents {
		if agent == handle {
			found = true
			continue
		}
		kept = append(kept, agent)
	}
	if !found {
		return false, nil
	}
	cfg.Agents = kept
	out, err := config.MarshalPreservingUnknowns(data, cfg)
	if err != nil {
		return false, err
	}
	if _, err := root.WriteFileAtomic("meta", "config.json", append(out, '\n'), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

type mailboxReleaseReport struct {
	Handle       string   `json:"handle"`
	Removed      bool     `json:"removed"`
	WaitingMail  int      `json:"waiting_mail,omitempty"`
	WaitingDirs  []string `json:"waiting_dirs,omitempty"`
	Deregistered bool     `json:"deregistered,omitempty"`
}

func writeMailboxReleaseReport(action string, report mailboxReleaseReport) error {
	if err := writeStdout("%s mailbox for %q\n", action, report.Handle); err != nil {
		return err
	}
	if report.Deregistered {
		if err := writeStdout("Removed %q from agents in meta/config.json.\n", report.Handle); err != nil {
			return err
		}
	}
	if len(report.WaitingDirs) > 0 {
		if err := writeStdout("Mail still waiting (%d message(s)):\n", report.WaitingMail); err != nil {
			return err
		}
		for _, dir := range report.WaitingDirs {
			if err := writeStdout("  %s\n", dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanupMailbox implements `amq cleanup --mailbox <handle>`: remove one
// agent's mailbox directory (and its config.json registration) after proving
// no mail is still waiting. Extension data under agents/<handle>/extensions/
// is layer-owned and goes with the directory (the layer-extension ADR: AMQ
// never inspects or preserves extension contents during an explicit removal
// the layer's caller requested). The reserved human handle is refused.
func cleanupMailbox(rootPath string, agent string, common *commonFlags, dryRun, yes bool) error {
	if agent == reservedHumanHandle {
		return UsageError("--mailbox: %q is the reserved human handle and cannot be removed", reservedHumanHandle)
	}
	if err := fsq.ValidateHandle(agent); err != nil {
		return UsageError("--mailbox: %v", err)
	}
	if !common.rootExplicit() {
		return UsageError("--mailbox requires an explicit --root <session-root>")
	}
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	agentRel := filepath.Join("agents", agent)
	if info, err := root.Stat(agentRel); err != nil || !info.IsDir() {
		if common.JSON {
			return writeJSON(os.Stdout, mailboxReleaseReport{Handle: agent, Removed: false})
		}
		return writeStdout("No mailbox for %q at %s.\n", agent, filepath.Join(root.Base(), agentRel))
	}

	// Refuse while mail waits. inbox/new, inbox/tmp, dlq/new, dlq/tmp: any
	// non-dot file there is undelivered or failed-unresolved mail whose
	// consumer has not seen it.
	totalWaiting := 0
	var waitingDirs []string
	for _, leaf := range []string{"inbox/new", "inbox/tmp", "dlq/new", "dlq/tmp"} {
		rel := filepath.Join(agentRel, filepath.FromSlash(leaf))
		count, err := countWaitingMail(root, rel)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", rel, err)
		}
		if count > 0 {
			totalWaiting += count
			waitingDirs = append(waitingDirs, filepath.Join(root.Base(), rel))
		}
	}
	if totalWaiting > 0 {
		if common.JSON {
			if err := writeJSON(os.Stdout, mailboxReleaseReport{
				Handle: agent, Removed: false, WaitingMail: totalWaiting, WaitingDirs: waitingDirs,
			}); err != nil {
				return err
			}
			// The JSON report is the payload; the exit code still says action
			// required so scripted callers (Amit's releaseChatMailbox) see it.
			return ActionRequiredError(
				"mailbox %q still holds %d waiting message(s); drain the inbox or purge the DLQ first",
				agent, totalWaiting,
			)
		}
		if err := writeMailboxReleaseReport("Keeping", mailboxReleaseReport{
			Handle: agent, Removed: false, WaitingMail: totalWaiting, WaitingDirs: waitingDirs,
		}); err != nil {
			return err
		}
		return ActionRequiredError(
			"mailbox %q still holds %d waiting message(s); drain the inbox or purge the DLQ first",
			agent, totalWaiting,
		)
	}

	// outbox/sent and receipts are historical records of what this agent SENT
	// and DELIVERED; dlq/cur is resolved failures. They do not block removal.
	report := mailboxReleaseReport{Handle: agent, Removed: true}
	if dryRun {
		if common.JSON {
			return writeJSON(os.Stdout, report)
		}
		return writeMailboxReleaseReport("Would remove", report)
	}
	if !yes {
		if err := writeMailboxReleaseReport("Will remove", report); err != nil {
			return err
		}
		confirmed, err := confirmPrompt(fmt.Sprintf("Remove mailbox %q and its config.json registration?", agent))
		if err != nil {
			return err
		}
		if !confirmed {
			return writeStdoutLine("Aborted.")
		}
	}

	// Deregister under the config lock BEFORE the directory goes, so a
	// concurrent sender that reads config.json never routes to a mailbox
	// that is being removed.
	if err := root.WithConfigLock(func(r *fsq.DeliveryRoot) error {
		removed, err := mailboxRemoveAgentLocked(r, agent)
		report.Deregistered = removed
		return err
	}); err != nil {
		return err
	}
	if err := root.RemoveAll(agentRel); err != nil {
		return fmt.Errorf("remove %s: %w", filepath.Join(root.Base(), agentRel), err)
	}

	if common.JSON {
		return writeJSON(os.Stdout, report)
	}
	return writeMailboxReleaseReport("Removed", report)
}
