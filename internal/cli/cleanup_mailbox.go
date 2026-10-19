package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// mailboxCleanupReport is the one JSON shape for every cleanup --mailbox
// outcome. Refusal is empty unless the command exits with action required.
type mailboxCleanupReport struct {
	Handle    string             `json:"handle"`
	Root      string             `json:"root"`
	Removed   bool               `json:"removed"`
	Absent    bool               `json:"absent"`
	DryRun    bool               `json:"dry_run,omitempty"`
	Refusal   string             `json:"refusal,omitempty"`
	Wake      string             `json:"wake,omitempty"`
	Inbox     *mailboxLeafCounts `json:"inbox,omitempty"`
	DLQ       *mailboxLeafCounts `json:"dlq,omitempty"`
	Path      string             `json:"path,omitempty"`
	Preserved string             `json:"preserved,omitempty"`
}

type mailboxLeafCounts struct {
	Tmp int `json:"tmp"`
	New int `json:"new"`
	Cur int `json:"cur"`
}

func (c mailboxLeafCounts) total() int { return c.Tmp + c.New + c.Cur }

func (c mailboxLeafCounts) String() string {
	return fmt.Sprintf("tmp=%d new=%d cur=%d", c.Tmp, c.New, c.Cur)
}

const (
	mailboxRefusalMail      = "mail_present"
	mailboxRefusalWake      = "wake_live"
	mailboxRefusalUnsafe    = "unsafe_path"
	mailboxRefusalPreserved = "preserved"
)

// mailboxUnsafePathError reports a symlink or non-directory where the
// mailbox layout requires a real directory.
type mailboxUnsafePathError struct {
	Path   string
	Reason string
}

func (e *mailboxUnsafePathError) Error() string {
	return fmt.Sprintf("%s %s", e.Path, e.Reason)
}

// cleanupMailbox removes <root>/agents/<handle> only when it holds no mail
// and no live or uncertain wake. The mailbox is renamed to a private sibling
// and checked again before deletion, so a message that lands during the
// removal is restored instead of lost.
func cleanupMailbox(rootPath, handle string, jsonOutput, dryRun, yes bool) error {
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	report := mailboxCleanupReport{Handle: handle, Root: root.Base(), DryRun: dryRun}
	refuse := func(refusal string, format string, args ...any) error {
		report.Refusal = refusal
		if jsonOutput {
			if err := writeJSON(os.Stdout, report); err != nil {
				return err
			}
		}
		return ActionRequiredError(format, args...)
	}
	refuseUnsafe := func(err error) error {
		var unsafe *mailboxUnsafePathError
		if errors.As(err, &unsafe) {
			return refuse(mailboxRefusalUnsafe, "refusing to remove mailbox %s: %v", handle, err)
		}
		return err
	}
	absent := func() error {
		report.Absent = true
		if jsonOutput {
			return writeJSON(os.Stdout, report)
		}
		return writeStdout("Mailbox %s is absent under %s; nothing to remove.\n", handle, report.Root)
	}

	if info, err := os.Lstat(root.Base()); err != nil {
		return err
	} else if info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, root.FileInfo()) {
		return refuse(mailboxRefusalUnsafe, "refusing to remove mailbox %s: root %s is a symlink", handle, root.Base())
	}
	if present, err := lstatMailboxDir(root, "agents"); err != nil {
		return refuseUnsafe(err)
	} else if !present {
		return absent()
	}
	agents, err := root.OpenDirectChild("agents")
	if err != nil {
		return err
	}
	defer func() { _ = agents.Close() }()
	if present, err := lstatMailboxDir(agents, handle); err != nil {
		return refuseUnsafe(err)
	} else if !present {
		return absent()
	}
	report.Path = agents.DisplayPath(handle)
	original, err := agents.Lstat(handle)
	if err != nil {
		return err
	}

	inbox, dlq, err := countPinnedMailbox(agents, handle, original)
	if err != nil {
		return refuseUnsafe(err)
	}
	report.Inbox, report.DLQ = &inbox, &dlq
	wake := inspectWakeLock(root.Base(), handle)
	report.Wake = string(wake.Status)
	if blocked := mailboxCleanupBlocked(inbox, dlq, wake); blocked != "" {
		return refuse(blocked, "%s", mailboxCleanupBlockedMessage(blocked, report))
	}
	if dryRun {
		if jsonOutput {
			return writeJSON(os.Stdout, report)
		}
		return writeStdout("Would remove empty mailbox %s\n", report.Path)
	}
	if !yes {
		confirmed, err := confirmPrompt(fmt.Sprintf(
			"Remove empty mailbox %s with its outbox, receipts, and extensions?",
			report.Path,
		))
		if err != nil {
			return err
		}
		if !confirmed {
			return writeStdoutLine("Aborted.")
		}
	}

	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("generate mailbox removal name: %w", err)
	}
	staging := ".removing-" + handle + "-" + hex.EncodeToString(random[:])
	if err := agents.RenameDirectChildNoReplace(handle, staging); err != nil {
		return fmt.Errorf("move mailbox %s aside: %w", report.Path, err)
	}
	// A writer that resolved the old path before the rename may have finished
	// into the moved tree. Check again; on any doubt put the mailbox back.
	inbox, dlq, err = countPinnedMailbox(agents, staging, original)
	if err == nil {
		report.Inbox, report.DLQ = &inbox, &dlq
		lockPath := filepath.Join(fsq.AgentBase(root.Base(), staging), wakeLockFileName)
		wake = inspectWakeLockWithReader(root.Base(), handle, lockPath, func() ([]byte, os.FileInfo, error) {
			return readWakeLockFileWithInfo(lockPath)
		})
		report.Wake = string(wake.Status)
		if blocked := mailboxCleanupBlocked(inbox, dlq, wake); blocked != "" {
			err = errors.New(mailboxCleanupBlockedMessage(blocked, report))
			report.Refusal = blocked
		}
	}
	if err != nil {
		if restoreErr := agents.RenameDirectChildNoReplace(staging, handle); restoreErr != nil {
			report.Preserved = agents.DisplayPath(staging)
			return refuse(
				mailboxRefusalPreserved,
				"%v; could not restore %s (%v), preserved at %s",
				err, report.Path, restoreErr, report.Preserved,
			)
		}
		if report.Refusal != "" {
			return refuse(report.Refusal, "%v", err)
		}
		return refuseUnsafe(err)
	}
	if err := agents.RemoveAllDirectChild(staging); err != nil {
		report.Preserved = agents.DisplayPath(staging)
		return fmt.Errorf("remove mailbox %s (moved to %s): %w", report.Path, report.Preserved, err)
	}
	report.Removed = true
	if jsonOutput {
		return writeJSON(os.Stdout, report)
	}
	return writeStdout("Removed empty mailbox %s\n", report.Path)
}

// lstatMailboxDir reports whether name exists as a real directory. A symlink
// or any other file type is unsafe.
func lstatMailboxDir(dir *fsq.DeliveryRoot, name string) (bool, error) {
	info, err := dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, &mailboxUnsafePathError{Path: dir.DisplayPath(name), Reason: "is a symlink"}
	}
	if !info.IsDir() {
		return false, &mailboxUnsafePathError{Path: dir.DisplayPath(name), Reason: "is not a directory"}
	}
	return true, nil
}

// countPinnedMailbox pins agents/<name>, proves it is the directory first
// observed as the mailbox, and counts entries in its inbox and DLQ leaves.
// tmp entries count because a write may be in flight.
func countPinnedMailbox(agents *fsq.DeliveryRoot, name string, original os.FileInfo) (inbox, dlq mailboxLeafCounts, err error) {
	mailbox, err := agents.OpenDirectChild(name)
	if err != nil {
		return inbox, dlq, err
	}
	defer func() { _ = mailbox.Close() }()
	if !os.SameFile(original, mailbox.FileInfo()) {
		return inbox, dlq, &mailboxUnsafePathError{Path: agents.DisplayPath(name), Reason: "changed while removing"}
	}
	if inbox, err = countMailboxLeaves(mailbox, "inbox"); err != nil {
		return inbox, dlq, err
	}
	dlq, err = countMailboxLeaves(mailbox, "dlq")
	return inbox, dlq, err
}

func countMailboxLeaves(mailbox *fsq.DeliveryRoot, parent string) (mailboxLeafCounts, error) {
	var counts mailboxLeafCounts
	if present, err := lstatMailboxDir(mailbox, parent); err != nil || !present {
		return counts, err
	}
	for _, leaf := range []struct {
		name  string
		count *int
	}{{"tmp", &counts.Tmp}, {"new", &counts.New}, {"cur", &counts.Cur}} {
		name := filepath.Join(parent, leaf.name)
		present, err := lstatMailboxDir(mailbox, name)
		if err != nil {
			return counts, err
		}
		if !present {
			continue
		}
		entries, err := mailbox.ReadDir(name)
		if err != nil {
			return counts, err
		}
		*leaf.count = len(entries)
	}
	return counts, nil
}

// mailboxCleanupBlocked names the refusal for held mail or a live or
// uncertain wake. Only a missing or proven-stale wake lock permits removal.
func mailboxCleanupBlocked(inbox, dlq mailboxLeafCounts, wake wakeLockInspection) string {
	if inbox.total() > 0 || dlq.total() > 0 {
		return mailboxRefusalMail
	}
	if wake.Exists && wake.Status != wakeLockStale {
		return mailboxRefusalWake
	}
	return ""
}

func mailboxCleanupBlockedMessage(refusal string, report mailboxCleanupReport) string {
	if refusal == mailboxRefusalWake {
		return fmt.Sprintf(
			"mailbox %s has a %s wake lock; stop the wake (see 'amq wake check --me %s --root %s') before removal",
			report.Handle, report.Wake, report.Handle, report.Root,
		)
	}
	return fmt.Sprintf(
		"mailbox %s holds mail (inbox %s; dlq %s); drain or recover it before removal",
		report.Handle, report.Inbox, report.DLQ,
	)
}
