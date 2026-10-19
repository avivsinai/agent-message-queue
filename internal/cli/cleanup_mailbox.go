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
// outcome. Refusal names why the command exited with action required;
// Preserved and Restored name where a mailbox tree was left.
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
	Path       string              `json:"path"`
	Recovered  []string            `json:"recovered,omitempty"`
	Restored   string              `json:"restored,omitempty"`
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

// mailboxState is what decides whether one mailbox tree may go.
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
	// drain and dlq purge never touch tmp; only the tmp sweep does.
	tmpSweep := fmt.Sprintf("wait for the write to finish, or remove stale tmp files with 'amq cleanup --tmp-older-than <duration> --root %s'", root)
	switch s.refusal() {
	case mailboxRefusalMail:
		if s.Inbox.New == 0 {
			return fmt.Sprintf("mailbox %s holds an unfinished inbox write (inbox tmp=%d); %s", handle, s.Inbox.Tmp, tmpSweep)
		}
		return fmt.Sprintf(
			"mailbox %s holds unread mail (inbox tmp=%d new=%d); run 'amq drain --me %s --root %s' before removal",
			handle, s.Inbox.Tmp, s.Inbox.New, handle, root,
		)
	case mailboxRefusalDLQ:
		if s.DLQ.New+s.DLQ.Cur == 0 {
			return fmt.Sprintf("mailbox %s holds an unfinished dead-letter write (dlq tmp=%d); %s", handle, s.DLQ.Tmp, tmpSweep)
		}
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

// stagedMailbox is a tree under fsq.MailboxRemovalDir left by an interrupted
// removal of the same handle.
type stagedMailbox struct {
	name  string
	state mailboxState
}

type mailboxCleanup struct {
	root   *fsq.DeliveryRoot
	handle string
	json   bool
	dryRun bool
	yes    bool
	report mailboxCleanupReport
}

// cleanupMailbox removes <root>/agents/<handle> only when it holds no unread
// mail, no dead letters, no extension data, and no live or uncertain wake.
// One per-root lock serializes removals. The mailbox is moved under
// meta/removing and checked twice before deletion, so a message that lands
// during the removal is restored instead of lost. Trees left by an earlier
// interrupted removal are removed when clean and restored to agents/<handle>
// when they hold mail, so the normal CLI can drain them.
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
	c := &mailboxCleanup{root: root, handle: handle, json: jsonOutput, dryRun: dryRun, yes: yes}
	c.report = mailboxCleanupReport{
		Handle: handle, Root: root.Base(), DryRun: dryRun,
		Path: root.DisplayPath(filepath.Join("agents", handle)),
	}
	return root.WithMailboxRemovalLock(c.run)
}

func (c *mailboxCleanup) run() error {
	agents, err := openMailboxDirIfPresent(c.root, "agents")
	if err != nil {
		return c.refuseUnsafe(err)
	}
	if agents != nil {
		defer func() { _ = agents.Close() }()
	}
	present := false
	if agents != nil {
		if present, err = lstatMailboxDir(agents, c.handle); err != nil {
			return c.refuseUnsafe(err)
		}
	}
	c.report.Absent = !present
	var live mailboxState
	var original os.FileInfo
	if present {
		if original, err = agents.Lstat(c.handle); err != nil {
			return err
		}
		if live, err = inspectMailboxDir(c.root, agents, c.handle, c.handle, original); err != nil {
			return c.refuseUnsafe(err)
		}
		c.record(live)
	}

	removing, err := openMailboxRemovalDir(c.root, false)
	if err != nil {
		return c.refuseUnsafe(err)
	}
	if removing != nil {
		defer func() { _ = removing.Close() }()
	}
	clean, dirty, err := c.findStaged(removing)
	if err != nil {
		return c.refuseUnsafe(err)
	}

	if refusal := live.refusal(); present && refusal != "" {
		message := live.refusalMessage(c.handle, c.report.Root)
		if len(dirty) > 0 {
			c.report.Preserved = removing.DisplayPath(dirty[0].name)
			message += fmt.Sprintf(
				"; an interrupted removal also preserved %s: drain %s, then run 'amq cleanup --mailbox %s --root %s' again",
				c.report.Preserved, c.handle, c.handle, c.report.Root,
			)
		}
		return c.refuse(refusal, "%s", message)
	}
	if !present && len(clean)+len(dirty) == 0 {
		return c.finish()
	}
	if c.dryRun {
		for _, staged := range clean {
			c.report.Recovered = append(c.report.Recovered, removing.DisplayPath(staged.name))
		}
		if len(dirty) > 0 {
			c.record(dirty[0].state)
			c.report.Preserved = removing.DisplayPath(dirty[0].name)
			return c.refuse(dirty[0].state.refusal(), "%s; it would be restored from %s",
				dirty[0].state.refusalMessage(c.handle, c.report.Root), c.report.Preserved)
		}
		return c.finish()
	}
	if !c.yes {
		prompt := fmt.Sprintf("Remove empty mailbox %s with its outbox and receipts (read history in inbox/cur included)?", c.report.Path)
		if !present {
			prompt = fmt.Sprintf("Finish %d interrupted removal(s) of mailbox %s?", len(clean)+len(dirty), c.handle)
		}
		confirmed, err := confirmPrompt(prompt)
		if err != nil {
			return err
		}
		if !confirmed {
			return writeStdoutLine("Aborted.")
		}
	}

	if present {
		if removing == nil {
			if removing, err = openMailboxRemovalDir(c.root, true); err != nil {
				return c.refuseUnsafe(err)
			}
			defer func() { _ = removing.Close() }()
		}
		if err := c.removeLive(removing, original); err != nil {
			return err
		}
	}
	for _, staged := range clean {
		if err := removing.RemoveAllDirectChild(staged.name); err != nil {
			return fmt.Errorf("remove interrupted mailbox removal %s: %w", removing.DisplayPath(staged.name), err)
		}
		c.report.Recovered = append(c.report.Recovered, removing.DisplayPath(staged.name))
	}
	if len(dirty) > 0 {
		return c.restore(removing, dirty)
	}
	return c.finish()
}

// removeLive moves agents/<handle> under meta/removing, checks it twice, and
// deletes it. Anything that arrived after the move puts the mailbox back; a
// recreated name is never overwritten and the moved tree is kept instead.
func (c *mailboxCleanup) removeLive(removing *fsq.DeliveryRoot, original os.FileInfo) error {
	staging, err := fsq.NewMailboxRemovalName(c.handle)
	if err != nil {
		return err
	}
	agentPath := filepath.Join("agents", c.handle)
	stagingPath := filepath.Join(fsq.MailboxRemovalDir, staging)
	if err := c.root.RenameNoReplace(agentPath, stagingPath); err != nil {
		return fmt.Errorf("move mailbox %s aside: %w", c.report.Path, err)
	}
	var state mailboxState
	for check := 0; check < 2 && err == nil && state.refusal() == ""; check++ {
		if check > 0 {
			time.Sleep(mailboxRemovalSettle)
		}
		state, err = inspectMailboxDir(c.root, removing, c.handle, staging, original)
	}
	if err == nil && state.refusal() == "" {
		if err := removing.RemoveAllDirectChild(staging); err != nil {
			return fmt.Errorf("remove mailbox %s (moved to %s): %w", c.report.Path, removing.DisplayPath(staging), err)
		}
		c.report.Removed = true
		return nil
	}
	cause := err
	if cause == nil {
		c.record(state)
		cause = errors.New(state.refusalMessage(c.handle, c.report.Root))
	}
	if restoreErr := c.root.RenameNoReplace(stagingPath, agentPath); restoreErr != nil {
		c.report.Preserved = removing.DisplayPath(staging)
		return c.refuse(mailboxRefusalPreserved, "%v; could not restore %s (%v), preserved at %s; run cleanup --mailbox %s again to restore it",
			cause, c.report.Path, restoreErr, c.report.Preserved, c.handle)
	}
	if err != nil {
		return c.refuseUnsafe(err)
	}
	return c.refuse(state.refusal(), "%v", cause)
}

// restore renames the first staged tree that holds mail back to
// agents/<handle> so the normal CLI can drain it, then refuses with its
// counts.
func (c *mailboxCleanup) restore(removing *fsq.DeliveryRoot, dirty []stagedMailbox) error {
	staged := dirty[0]
	if _, err := lstatMailboxDir(c.root, "agents"); err != nil {
		return c.refuseUnsafe(err)
	}
	agents, err := c.root.OpenOrCreateDirectChild("agents", 0o700)
	if err != nil {
		return err
	}
	_ = agents.Close()
	stagedPath := removing.DisplayPath(staged.name)
	if err := c.root.RenameNoReplace(filepath.Join(fsq.MailboxRemovalDir, staged.name), filepath.Join("agents", c.handle)); err != nil {
		c.report.Preserved = stagedPath
		return c.refuse(mailboxRefusalPreserved, "%s; could not restore it from %s to %s (%v)",
			staged.state.refusalMessage(c.handle, c.report.Root), stagedPath, c.report.Path, err)
	}
	c.record(staged.state)
	c.report.Restored = c.report.Path
	if len(dirty) > 1 {
		c.report.Preserved = removing.DisplayPath(dirty[1].name)
	}
	return c.refuse(staged.state.refusal(), "%s; restored from an interrupted removal at %s",
		staged.state.refusalMessage(c.handle, c.report.Root), stagedPath)
}

// findStaged inspects every tree an interrupted removal of handle left under
// meta/removing. The removal lock guarantees none belongs to a live run.
func (c *mailboxCleanup) findStaged(removing *fsq.DeliveryRoot) (clean, dirty []stagedMailbox, err error) {
	if removing == nil {
		return nil, nil, nil
	}
	entries, err := removing.ReadDir(".")
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if owner, ok := fsq.ParseMailboxRemovalName(entry.Name()); !ok || owner != c.handle {
			continue
		}
		if present, err := lstatMailboxDir(removing, entry.Name()); err != nil {
			return nil, nil, err
		} else if !present {
			continue
		}
		state, err := inspectMailboxDir(c.root, removing, c.handle, entry.Name(), nil)
		if err != nil {
			return nil, nil, err
		}
		if state.refusal() == "" {
			clean = append(clean, stagedMailbox{name: entry.Name(), state: state})
		} else {
			dirty = append(dirty, stagedMailbox{name: entry.Name(), state: state})
		}
	}
	return clean, dirty, nil
}

func (c *mailboxCleanup) refuse(refusal string, format string, args ...any) error {
	c.report.Refusal = refusal
	if c.json {
		if err := writeJSON(os.Stdout, c.report); err != nil {
			return err
		}
	}
	return ActionRequiredError(format, args...)
}

func (c *mailboxCleanup) refuseUnsafe(err error) error {
	var unsafe *mailboxUnsafePathError
	if errors.As(err, &unsafe) {
		return c.refuse(mailboxRefusalUnsafe, "refusing to remove mailbox %s: %v", c.handle, err)
	}
	return err
}

func (c *mailboxCleanup) record(state mailboxState) {
	inbox, dlq := state.Inbox, state.DLQ
	c.report.Inbox, c.report.DLQ, c.report.Extensions = &inbox, &dlq, state.Extensions
	c.report.Wake = string(state.Wake.Status)
}

func (c *mailboxCleanup) finish() error {
	if c.json {
		return writeJSON(os.Stdout, c.report)
	}
	verb := "Removed"
	if c.dryRun {
		verb = "Would remove"
	}
	for _, path := range c.report.Recovered {
		if err := writeStdout("%s interrupted mailbox removal %s\n", verb, path); err != nil {
			return err
		}
	}
	if c.report.Absent {
		return writeStdout("Mailbox %s is absent under %s.\n", c.handle, c.report.Root)
	}
	return writeStdout("%s empty mailbox %s\n", verb, c.report.Path)
}

// openMailboxDirIfPresent pins parent/name when it is a real directory and
// returns nil when it does not exist.
func openMailboxDirIfPresent(parent *fsq.DeliveryRoot, name string) (*fsq.DeliveryRoot, error) {
	present, err := lstatMailboxDir(parent, name)
	if err != nil || !present {
		return nil, err
	}
	return parent.OpenDirectChild(name)
}

// openMailboxRemovalDir pins meta/removing, creating it (mode 0700) when
// create is set. It returns nil when it is absent and create is not set.
func openMailboxRemovalDir(root *fsq.DeliveryRoot, create bool) (*fsq.DeliveryRoot, error) {
	parentName, name := filepath.Split(fsq.MailboxRemovalDir)
	parentName = filepath.Clean(parentName)
	var parent *fsq.DeliveryRoot
	var err error
	if create {
		if _, err = lstatMailboxDir(root, parentName); err != nil {
			return nil, err
		}
		parent, err = root.OpenOrCreateDirectChild(parentName, 0o700)
	} else {
		parent, err = openMailboxDirIfPresent(root, parentName)
	}
	if err != nil || parent == nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	if !create {
		return openMailboxDirIfPresent(parent, name)
	}
	if _, err := lstatMailboxDir(parent, name); err != nil {
		return nil, err
	}
	return parent.OpenOrCreateDirectChild(name, 0o700)
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

// inspectMailboxDir pins parent/<name>, proves it is the directory first
// observed as the mailbox when original is set, and reads everything that
// decides whether it may go. tmp entries count because a write may be in
// flight.
func inspectMailboxDir(root, parent *fsq.DeliveryRoot, handle, name string, original os.FileInfo) (mailboxState, error) {
	var state mailboxState
	mailbox, err := parent.OpenDirectChild(name)
	if err != nil {
		return state, err
	}
	defer func() { _ = mailbox.Close() }()
	if original != nil && !os.SameFile(original, mailbox.FileInfo()) {
		return state, &mailboxUnsafePathError{Path: parent.DisplayPath(name), Reason: "changed while removing"}
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
	lockPath := filepath.Join(mailbox.Base(), wakeLockFileName)
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

// checkMailboxRemovalHints reports trees under meta/removing: mailboxes an
// interrupted removal left behind.
func checkMailboxRemovalHints(root string) []opsHint {
	dir := filepath.Join(root, fsq.MailboxRemovalDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var hints []opsHint
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		message := fmt.Sprintf("%s is not a tree that cleanup --mailbox made; inspect it", path)
		if handle, ok := fsq.ParseMailboxRemovalName(entry.Name()); ok {
			message = fmt.Sprintf(
				"Interrupted mailbox removal for %s left %s; run 'amq cleanup --mailbox %s --root %s' to remove it, or to restore it to agents/%s when it holds mail",
				handle, path, handle, root, handle,
			)
		}
		hints = append(hints, opsHint{Code: "mailbox_removal_left", Status: "warn", Message: message})
	}
	return hints
}
