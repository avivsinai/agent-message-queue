package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// mailboxRemovalSettle is the wait between the two checks of a moved
// mailbox. Delivery publishes through directory handles opened before the
// rename, so a sender already inside that step can still land in the moved
// tree shortly after the first check.
const mailboxRemovalSettle = 250 * time.Millisecond

// mailboxCleanupReport is the one JSON shape for every cleanup --mailbox
// outcome. Refusal is empty unless the command exits with action required.
type mailboxCleanupReport struct {
	Handle     string              `json:"handle"`
	Root       string              `json:"root"`
	Removed    bool                `json:"removed"`
	Absent     bool                `json:"absent"`
	DryRun     bool                `json:"dry_run,omitempty"`
	Refusal    string              `json:"refusal,omitempty"`
	Wake       string              `json:"wake,omitempty"`
	Inbox      *mailboxInboxCounts `json:"inbox,omitempty"`
	DLQ        *mailboxDLQCounts   `json:"dlq,omitempty"`
	Extensions []string            `json:"extensions,omitempty"`
	Path       string              `json:"path,omitempty"`
	Recovered  []string            `json:"recovered,omitempty"`
	Preserved  string              `json:"preserved,omitempty"`
}

// mailboxInboxCounts counts unread inbox entries. inbox/cur is read history
// and is removed with the mailbox, so it is not counted.
type mailboxInboxCounts struct {
	Tmp int `json:"tmp"`
	New int `json:"new"`
}

type mailboxDLQCounts struct {
	Tmp int `json:"tmp"`
	New int `json:"new"`
	Cur int `json:"cur"`
}

const (
	mailboxRefusalMail       = "mail_present"
	mailboxRefusalDLQ        = "dlq_present"
	mailboxRefusalExtensions = "extensions_present"
	mailboxRefusalWake       = "wake_live"
	mailboxRefusalUnsafe     = "unsafe_path"
	mailboxRefusalPreserved  = "preserved"
)

// mailboxState is what decides whether one mailbox directory may go.
type mailboxState struct {
	Inbox      mailboxInboxCounts
	DLQ        mailboxDLQCounts
	Extensions []string
	Wake       wakeLockInspection
}

// refusal names what keeps the mailbox. Only a missing or proven-stale wake
// lock permits removal.
func (s mailboxState) refusal() string {
	switch {
	case s.Inbox.Tmp+s.Inbox.New > 0:
		return mailboxRefusalMail
	case s.DLQ.Tmp+s.DLQ.New+s.DLQ.Cur > 0:
		return mailboxRefusalDLQ
	case len(s.Extensions) > 0:
		return mailboxRefusalExtensions
	case s.Wake.Exists && s.Wake.Status != wakeLockStale:
		return mailboxRefusalWake
	}
	return ""
}

func (s mailboxState) refusalMessage(handle, root string) string {
	switch s.refusal() {
	case mailboxRefusalMail:
		return fmt.Sprintf(
			"mailbox %s holds unread mail (inbox tmp=%d new=%d); run 'amq drain --me %s --root %s' before removal",
			handle, s.Inbox.Tmp, s.Inbox.New, handle, root,
		)
	case mailboxRefusalDLQ:
		return fmt.Sprintf(
			"mailbox %s holds dead letters (dlq tmp=%d new=%d cur=%d); inspect them with 'amq dlq list --me %s --root %s', then retry or run 'amq dlq purge --me %s --root %s' before removal",
			handle, s.DLQ.Tmp, s.DLQ.New, s.DLQ.Cur, handle, root, handle, root,
		)
	case mailboxRefusalExtensions:
		return fmt.Sprintf(
			"mailbox %s holds extension data (%s); the owning layer removes its extension data first",
			handle, strings.Join(s.Extensions, ", "),
		)
	case mailboxRefusalWake:
		return fmt.Sprintf(
			"mailbox %s has a %s wake lock; stop the wake (see 'amq wake check --me %s --root %s') before removal",
			handle, s.Wake.Status, handle, root,
		)
	}
	return ""
}

// mailboxUnsafePathError reports a symlink or non-directory where the
// mailbox layout requires a real directory.
type mailboxUnsafePathError struct {
	Path   string
	Reason string
}

func (e *mailboxUnsafePathError) Error() string {
	return fmt.Sprintf("%s %s", e.Path, e.Reason)
}

// cleanupMailbox removes <root>/agents/<handle> only when it holds no unread
// mail, no dead letters, no extension data, and no live or uncertain wake.
// The mailbox is moved to a private sibling and checked twice before
// deletion, so a message that lands during the removal is restored instead
// of lost. Siblings left by an earlier interrupted removal are finished the
// same way.
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
	record := func(state mailboxState) {
		inbox, dlq := state.Inbox, state.DLQ
		report.Inbox, report.DLQ, report.Extensions = &inbox, &dlq, state.Extensions
		report.Wake = string(state.Wake.Status)
	}
	// checkOrphan refuses when a sibling left by an earlier removal still
	// holds anything that blocks removal.
	checkOrphan := func(agents *fsq.DeliveryRoot, orphan string) error {
		state, err := inspectMailboxDir(root, agents, handle, orphan, nil)
		if err != nil {
			return refuseUnsafe(err)
		}
		if refusal := state.refusal(); refusal != "" {
			record(state)
			report.Preserved = agents.DisplayPath(orphan)
			return refuse(refusal, "an earlier removal of mailbox %s preserved %s, which still holds data (%s); recover it by hand",
				handle, report.Preserved, refusal)
		}
		return nil
	}
	finish := func() error {
		if jsonOutput {
			return writeJSON(os.Stdout, report)
		}
		return writeMailboxCleanupReport(report)
	}

	if present, err := lstatMailboxDir(root, "agents"); err != nil {
		return refuseUnsafe(err)
	} else if !present {
		report.Absent = true
		return finish()
	}
	agents, err := root.OpenDirectChild("agents")
	if err != nil {
		return err
	}
	defer func() { _ = agents.Close() }()

	orphans, err := findMailboxRemovalOrphans(agents, handle)
	if err != nil {
		return refuseUnsafe(err)
	}
	for _, orphan := range orphans {
		if err := checkOrphan(agents, orphan); err != nil {
			return err
		}
	}

	present, err := lstatMailboxDir(agents, handle)
	if err != nil {
		return refuseUnsafe(err)
	}
	var original os.FileInfo
	if present {
		report.Path = agents.DisplayPath(handle)
		if original, err = agents.Lstat(handle); err != nil {
			return err
		}
		state, err := inspectMailboxDir(root, agents, handle, handle, original)
		if err != nil {
			return refuseUnsafe(err)
		}
		record(state)
		if refusal := state.refusal(); refusal != "" {
			return refuse(refusal, "%s", state.refusalMessage(handle, report.Root))
		}
	} else {
		report.Absent = true
	}
	if dryRun {
		for _, orphan := range orphans {
			report.Recovered = append(report.Recovered, agents.DisplayPath(orphan))
		}
		return finish()
	}
	if !present && len(orphans) == 0 {
		return finish()
	}
	if !yes {
		target := report.Path
		if !present {
			target = fmt.Sprintf("left by %d interrupted removal(s) of %s", len(orphans), handle)
		}
		confirmed, err := confirmPrompt(fmt.Sprintf(
			"Remove empty mailbox %s with its outbox and receipts (read history in inbox/cur included)?", target,
		))
		if err != nil {
			return err
		}
		if !confirmed {
			return writeStdoutLine("Aborted.")
		}
	}

	for _, orphan := range orphans {
		if err := checkOrphan(agents, orphan); err != nil {
			return err
		}
		if err := agents.RemoveAllDirectChild(orphan); err != nil {
			return fmt.Errorf("remove preserved mailbox %s: %w", agents.DisplayPath(orphan), err)
		}
		report.Recovered = append(report.Recovered, agents.DisplayPath(orphan))
	}
	if !present {
		return finish()
	}

	staging, err := fsq.NewMailboxRemovalName(handle)
	if err != nil {
		return err
	}
	if err := agents.RenameDirectChildNoReplace(handle, staging); err != nil {
		return fmt.Errorf("move mailbox %s aside: %w", report.Path, err)
	}
	var state mailboxState
	for check := 0; check < 2 && err == nil && state.refusal() == ""; check++ {
		if check > 0 {
			time.Sleep(mailboxRemovalSettle)
		}
		state, err = inspectMailboxDir(root, agents, handle, staging, original)
	}
	if err != nil {
		state = mailboxState{}
	}
	if err == nil {
		record(state)
		if state.refusal() == "" {
			if err := agents.RemoveAllDirectChild(staging); err != nil {
				return fmt.Errorf("remove mailbox %s (moved to %s): %w",
					report.Path, agents.DisplayPath(staging), err)
			}
			report.Removed = true
			return finish()
		}
		err = errors.New(state.refusalMessage(handle, report.Root))
	}
	// Something arrived or changed after the move: put the mailbox back. A
	// recreated name is never overwritten; the moved tree is kept instead.
	if restoreErr := agents.RenameDirectChildNoReplace(staging, handle); restoreErr != nil {
		report.Preserved = agents.DisplayPath(staging)
		return refuse(mailboxRefusalPreserved, "%v; could not restore %s (%v), preserved at %s",
			err, report.Path, restoreErr, report.Preserved)
	}
	if refusal := state.refusal(); refusal != "" {
		return refuse(refusal, "%v", err)
	}
	return refuseUnsafe(err)
}

func writeMailboxCleanupReport(report mailboxCleanupReport) error {
	verb := "Removed"
	if report.DryRun {
		verb = "Would remove"
	}
	for _, path := range report.Recovered {
		if err := writeStdout("%s mailbox left by an interrupted removal: %s\n", verb, path); err != nil {
			return err
		}
	}
	if report.Absent {
		return writeStdout("Mailbox %s is absent under %s.\n", report.Handle, report.Root)
	}
	return writeStdout("%s empty mailbox %s\n", verb, report.Path)
}

// findMailboxRemovalOrphans lists agents/.removing-<handle>-<nonce> entries
// that an interrupted removal of handle left behind.
func findMailboxRemovalOrphans(agents *fsq.DeliveryRoot, handle string) ([]string, error) {
	entries, err := agents.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, entry := range entries {
		if owner, ok := fsq.ParseMailboxRemovalName(entry.Name()); !ok || owner != handle {
			continue
		}
		if present, err := lstatMailboxDir(agents, entry.Name()); err != nil {
			return nil, err
		} else if present {
			orphans = append(orphans, entry.Name())
		}
	}
	return orphans, nil
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

// inspectMailboxDir pins agents/<name>, proves it is the directory first
// observed as the mailbox when original is set, and reads everything that
// decides whether it may go. tmp entries count because a write may be in
// flight.
func inspectMailboxDir(root, agents *fsq.DeliveryRoot, handle, name string, original os.FileInfo) (mailboxState, error) {
	var state mailboxState
	mailbox, err := agents.OpenDirectChild(name)
	if err != nil {
		return state, err
	}
	defer func() { _ = mailbox.Close() }()
	if original != nil && !os.SameFile(original, mailbox.FileInfo()) {
		return state, &mailboxUnsafePathError{Path: agents.DisplayPath(name), Reason: "changed while removing"}
	}
	for _, c := range []struct {
		path  []string
		count *int
	}{
		{[]string{"inbox", "tmp"}, &state.Inbox.Tmp},
		{[]string{"inbox", "new"}, &state.Inbox.New},
		{[]string{"dlq", "tmp"}, &state.DLQ.Tmp},
		{[]string{"dlq", "new"}, &state.DLQ.New},
		{[]string{"dlq", "cur"}, &state.DLQ.Cur},
	} {
		entries, err := listMailboxEntries(mailbox, c.path...)
		if err != nil {
			return state, err
		}
		*c.count = len(entries)
	}
	if state.Extensions, err = listMailboxEntries(mailbox, "extensions"); err != nil {
		return state, err
	}
	lockPath := filepath.Join(fsq.AgentBase(root.Base(), name), wakeLockFileName)
	state.Wake = inspectWakeLockWithReader(root.Base(), handle, lockPath, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileWithInfo(lockPath)
	})
	return state, nil
}

// listMailboxEntries names the entries of one mailbox directory, refusing a
// symlink or non-directory at any component. A missing directory is empty.
func listMailboxEntries(mailbox *fsq.DeliveryRoot, parts ...string) ([]string, error) {
	for i := range parts {
		if present, err := lstatMailboxDir(mailbox, filepath.Join(parts[:i+1]...)); err != nil || !present {
			return nil, err
		}
	}
	entries, err := mailbox.ReadDir(filepath.Join(parts...))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

// checkMailboxRemovalHints reports agents/.removing-* entries: mailboxes an
// interrupted or refused removal left behind.
func checkMailboxRemovalHints(root string) []opsHint {
	entries, err := os.ReadDir(filepath.Join(root, "agents"))
	if err != nil {
		return nil
	}
	var hints []opsHint
	for _, entry := range entries {
		if !fsq.IsMailboxRemovalStaging(entry.Name()) {
			continue
		}
		path := filepath.Join(root, "agents", entry.Name())
		message := fmt.Sprintf("Mailbox removal left %s; inspect it and remove it by hand", path)
		if handle, ok := fsq.ParseMailboxRemovalName(entry.Name()); ok {
			message = fmt.Sprintf(
				"Mailbox removal for %s left %s; run 'amq cleanup --mailbox %s --root %s' to finish it, or recover its mail first",
				handle, path, handle, root,
			)
		}
		hints = append(hints, opsHint{Code: "mailbox_removal_left", Status: "warn", Message: message})
	}
	return hints
}
