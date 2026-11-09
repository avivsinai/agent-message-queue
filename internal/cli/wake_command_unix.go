//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/wakecmd"
)

// wakeCommandPickerPause bounds how long the queue waits after a command
// that may open a picker.
const wakeCommandPickerPause = 60 * time.Second

// maxWakeCommandEntries bounds one listing of the queue directory, which
// also keeps an hour of outcomes and event keys.
const maxWakeCommandEntries = 4096

// wakeCommands is the wake's side of its seat's owner command queue
// (wakecmd). The queue is read only through the wake's retained agent
// directory.
type wakeCommands struct {
	dir *wakeAgentDir
	// typing is the claimed command being typed: its event is taken, and
	// its text may be in the composer with its Enter still owed. Zero when
	// there is none.
	typing wakecmd.Request
	// settle counts the ticks with no dialog still owed before the next
	// command is typed.
	settle int
	// pausedAt is when a command that may open a picker was typed; zero
	// when none was.
	pausedAt time.Time
}

// serveWakeCommands types the owner's oldest queued slash command into the
// terminal, as its own line followed by Enter, by the doorbell's injection
// mechanism and mode. A command is typed only when the owner's signed
// message it carries verifies for this seat (wakecmd.Verify), and each
// signed message at most once (dialog.TakeCommand); its Enter is sent only
// while it still verifies. It waits, queued, while a dialog may be open,
// while the terminal still owes earlier input, for one full tick with no
// dialog after the previous command, and after a command that may open a
// picker; it expires wakecmd.Expiry after the owner signed it. The
// harness's composer queues typed input during a turn, so a turn in
// progress does not hold it. A seat that cannot take commands answers each
// one Refused.
func serveWakeCommands(cfg *wakeConfig) {
	c := cfg.commands
	if c == nil {
		return
	}
	q, ok, err := openWakeCommandQueue(c.dir)
	if err != nil {
		_ = writeWakeDiagnostic(cfg, "amq wake: read the command queue: %v\n", err)
		return
	}
	if !ok {
		return
	}
	defer func() { _ = q.Close() }()
	qfd := int(q.Fd())
	entries, err := readWakeCommandRequests(q)
	if err != nil {
		_ = writeWakeDiagnostic(cfg, "amq wake: read the command queue: %v\n", err)
		return
	}
	if len(entries) == 0 && c.typing.ID == "" {
		return
	}
	if why := wakeCommandRefusal(cfg); why != "" {
		for _, e := range entries {
			refuseWakeCommand(cfg, qfd, e.req.ID, why)
		}
		return
	}
	now := cfg.wakeDoorbellNow()
	name, seat, seatErr := wakeCommandSeat(cfg.root, cfg.me)
	var reqs []wakecmd.Request
	var events []string
	for _, e := range entries {
		event, err := "", errors.Join(e.err, seatErr)
		if err == nil {
			event, err = wakecmd.Verify(e.req, seat, now)
		}
		if err != nil {
			refuseWakeCommand(cfg, qfd, e.req.ID, err.Error())
			continue
		}
		reqs, events = append(reqs, e.req), append(events, event)
	}
	if c.typing.ID != "" {
		c.submitHeld(cfg, qfd, now)
		markWakeCommandsQueued(qfd, reqs, "an earlier command is being typed")
		return
	}
	if len(reqs) == 0 {
		return
	}
	if why := c.wait(cfg, reqs, now); why != "" {
		markWakeCommandsQueued(qfd, reqs, why)
		return
	}
	req, event := reqs[0], events[0]
	defer markWakeCommandsQueued(qfd, reqs[1:], "an earlier command is being typed")
	if wakeRenameNoReplaceAt(qfd, req.ID+wakecmd.ReqSuffix, qfd, req.ID+wakecmd.TypingSuffix) != nil {
		return
	}
	switch taken, err := dialog.TakeCommand(name, event, now, wakecmd.TakenKeep); {
	case err != nil:
		finishWakeCommand(cfg, qfd, req.ID, wakecmd.Refused, "the wake could not record the command: "+err.Error())
		return
	case !taken:
		finishWakeCommand(cfg, qfd, req.ID, wakecmd.Refused, "this message was already typed")
		return
	}
	c.typing = req
	notice := wakeNotification{
		input:  wakePayload{text: req.Text, provenance: wakePayloadOwnerCommand},
		output: wakePayload{text: "AMQ: the owner's command " + req.Text, provenance: wakePayloadSystemFixed},
	}
	deliveryErr := deliverWakeNotification(cfg, notice, true)
	switch {
	case c.typing.ID != req.ID:
		// It no longer verified before its Enter and was answered.
	case wakeInputAttemptConfirmed(cfg, deliveryErr):
		c.typing = wakecmd.Request{}
		c.typed(cfg, qfd, req, now)
	case cfg.inputDelivery.blocksDemotion():
		// Some of it may be in the composer, and its Enter was held: a
		// dialog opened in between. The Enter follows once no dialog is
		// open and the command still verifies.
		_ = markWakeCommand(qfd, req.ID, wakecmd.Queued, "a dialog opened before its Enter, so the text waits in the composer")
	default:
		// Nothing was written, so nothing is owed and the command can be
		// typed later.
		c.typing = wakecmd.Request{}
		clearWakeInputState(cfg)
		if deliveryErr != nil && cfg.lastInjectorOutcome != wakeInjectorOutcomeDeferred {
			why := "the wake could not type into the terminal (" + deliveryErr.Error() + ")"
			_ = writeWakeDiagnostic(cfg, "amq wake: owner command not typed: %s\n", why)
			finishWakeCommand(cfg, qfd, req.ID, wakecmd.Refused, why)
			return
		}
		if err := dialog.ForgetCommand(name, event); err != nil {
			finishWakeCommand(cfg, qfd, req.ID, wakecmd.Refused, "the wake could not type it and could not release it: "+err.Error())
			return
		}
		why := "the terminal is in use"
		if state, _ := cfg.dialogState(); state != wakeDialogClosed {
			why = "a dialog is open in the terminal"
		}
		_ = wakeRenameNoReplaceAt(qfd, req.ID+wakecmd.TypingSuffix, qfd, req.ID+wakecmd.ReqSuffix)
		_ = markWakeCommand(qfd, req.ID, wakecmd.Queued, why)
	}
}

// wait says why the next command must wait, or "".
func (c *wakeCommands) wait(cfg *wakeConfig, reqs []wakecmd.Request, now time.Time) string {
	if cfg.inputRecoveryRequired || cfg.inputDelivery.pending() {
		return "the terminal still owes earlier input"
	}
	if state, _ := cfg.dialogState(); state != wakeDialogClosed {
		return "a dialog is open in the terminal"
	}
	if c.settle > 0 {
		c.settle--
		return "an earlier command is being typed"
	}
	if !c.pausedAt.IsZero() {
		// The owner's next message ends the pause: by then the owner has
		// seen what the earlier command opened.
		newest := wakecmd.EnqueuedAt(reqs[len(reqs)-1].ID)
		if now.Sub(c.pausedAt) < wakeCommandPickerPause && !newest.After(c.pausedAt) {
			return "a picker may be open in the terminal"
		}
		c.pausedAt = time.Time{}
	}
	return ""
}

// typed records that req was typed with its Enter: the next command waits
// one full tick with no dialog, and longer after a picker.
func (c *wakeCommands) typed(cfg *wakeConfig, qfd int, req wakecmd.Request, now time.Time) {
	finishWakeCommand(cfg, qfd, req.ID, wakecmd.Typed, "")
	c.settle = 1
	if wakeCommandMayOpenPicker(req.Text) {
		c.pausedAt = now
	}
}

// submitHeld sends the owed Enter of the command being typed once no
// dialog is open; the delivery verifies it again first
// (ownerCommandMaySubmit).
func (c *wakeCommands) submitHeld(cfg *wakeConfig, qfd int, now time.Time) {
	if cfg.inputDelivery.pending() {
		if state, _ := cfg.dialogState(); state != wakeDialogClosed {
			return
		}
		if _, err := resumeWakeInputDelivery(cfg); err != nil {
			_ = writeWakeDiagnostic(cfg, "amq wake: owner command Enter not typed: %v\n", err)
			return
		}
		if c.typing.ID == "" || cfg.inputDelivery.pending() {
			return
		}
	}
	// The Enter was written, here or by the doorbell that resumed the same
	// input.
	req := c.typing
	c.typing = wakecmd.Request{}
	c.typed(cfg, qfd, req, now)
}

// ownerCommandMaySubmit reports whether the command being typed may get
// its Enter: it still verifies for this seat now. A command that does not
// is answered Refused and is no longer being typed; its text may stay in
// the composer, but no Enter follows it.
func ownerCommandMaySubmit(cfg *wakeConfig) bool {
	c := cfg.commands
	if c == nil || c.typing.ID == "" {
		return false
	}
	_, seat, err := wakeCommandSeat(cfg.root, cfg.me)
	if err == nil {
		_, err = wakecmd.Verify(c.typing, seat, cfg.wakeDoorbellNow())
	}
	if err == nil {
		return true
	}
	req := c.typing
	c.typing = wakecmd.Request{}
	q, ok, qerr := openWakeCommandQueue(c.dir)
	if qerr != nil || !ok {
		_ = writeWakeDiagnostic(cfg, "amq wake: owner command %s dropped before its Enter: %v\n", req.ID, err)
		return false
	}
	defer func() { _ = q.Close() }()
	finishWakeCommand(cfg, int(q.Fd()), req.ID, wakecmd.Refused, err.Error())
	return false
}

// wakeCommandMayOpenPicker reports whether text is a command that opens a
// picker in Claude Code. This is a heuristic from the commands known to
// open one; another command's picker is guarded only by the dialog marker
// and the tick with no dialog.
func wakeCommandMayOpenPicker(text string) bool {
	name, _, _ := strings.Cut(text, " ")
	return slices.Contains([]string{"/model", "/config", "/permissions", "/resume"}, name)
}

// wakeCommandRefusal says why this seat takes no commands, or "".
func wakeCommandRefusal(cfg *wakeConfig) string {
	switch {
	case cfg.injectMode == wakeInjectModeNone:
		return "the wake's inject mode is none, so it types nothing"
	case cfg.injectMode == wakeInjectModeClaudeNative:
		return "the wake delivers natively and types nothing"
	case cfg.harness != harness.Claude:
		return "only a Claude Code seat takes commands from Buzz"
	}
	return ""
}

// wakeCommandSeat is the binding that shares this seat (root, me): its
// name and what the wake trusts about it, the owner record its Buzz agent
// wrote. The record is trusted as the binding's dialog answers are: a file
// the Buzz agent writes in the user's binding store, outside the queue. It
// must name the binding's current session.
func wakeCommandSeat(root, me string) (string, wakecmd.Seat, error) {
	bindings, err := binding.List()
	if err != nil {
		return "", wakecmd.Seat{}, fmt.Errorf("read the Buzz shares: %w", err)
	}
	seat := mailboxSeat(root, me)
	for _, b := range bindings {
		if !b.Mailbox() || mailboxSeat(b.Root, b.Handle) != seat {
			continue
		}
		owner, err := dialog.ReadOwner(b.Name)
		if err != nil {
			return "", wakecmd.Seat{}, fmt.Errorf("read the owner of share %s: %w", b.Name, err)
		}
		if owner.Target != b.SessionIdentity() {
			return "", wakecmd.Seat{}, fmt.Errorf("the owner of share %s was recorded for another session", b.Name)
		}
		return b.Name, wakecmd.Seat{Target: owner.Target, Owner: owner.PubKey, Channel: owner.Channel}, nil
	}
	return "", wakecmd.Seat{}, errors.New("no Buzz share serves this session")
}

// mailboxSeat names the mailbox destination (root, handle), symlinks in
// root resolved.
func mailboxSeat(root, handle string) string {
	return binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: handle}.SessionIdentity()
}

// wakeCommandEntry is one request file: its parsed request, or why it is
// not one.
type wakeCommandEntry struct {
	req wakecmd.Request
	err error
}

// openWakeCommandQueue opens the command queue below the canonical agent
// directory: a directory, never through a symlink, mode 0700 and owned by
// this user. ok is false when there is no queue.
func openWakeCommandQueue(agentDir *wakeAgentDir) (q *os.File, ok bool, err error) {
	if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
		return nil, false, err
	}
	path := filepath.Join(agentDir.path, wakecmd.DirName)
	err = agentDir.withFD(func(dirfd int) error {
		fd, err := unix.Openat(dirfd, wakecmd.DirName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil
		case err != nil:
			return fmt.Errorf("open command queue %s: %w", path, err)
		}
		f := os.NewFile(uintptr(fd), path)
		info, err := f.Stat()
		if err == nil && info.Mode().Perm() != 0o700 {
			err = fmt.Errorf("command queue %s mode is %o, want 0700", path, info.Mode().Perm())
		}
		if err == nil {
			err = validateWakeTargetPathOwnership("command queue", path, info)
		}
		if err != nil {
			_ = f.Close()
			return err
		}
		q, ok = f, true
		return nil
	})
	return q, ok, err
}

// readWakeCommandRequests reads the unclaimed requests of q, oldest first.
// Each file is read without following a symlink and must be a regular file
// of this user, mode 0600.
func readWakeCommandRequests(q *os.File) ([]wakeCommandEntry, error) {
	names, err := q.Readdirnames(maxWakeCommandEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > maxWakeCommandEntries {
		return nil, fmt.Errorf("more than %d files in the command queue", maxWakeCommandEntries)
	}
	slices.Sort(names)
	var entries []wakeCommandEntry
	for _, name := range names {
		id, ok := wakecmd.RequestID(name)
		if !ok {
			continue
		}
		raw, _, exists, err := readWakeRepairMetadataAt(int(q.Fd()), name, "owner command",
			filepath.Join(q.Name(), name), wakecmd.MaxRequestBytes)
		if !exists {
			continue
		}
		e := wakeCommandEntry{req: wakecmd.Request{ID: id}, err: err}
		if err == nil {
			e.req, e.err = wakecmd.Parse(id, raw)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// refuseWakeCommand claims request id and answers it Refused.
func refuseWakeCommand(cfg *wakeConfig, qfd int, id, why string) {
	if wakeRenameNoReplaceAt(qfd, id+wakecmd.ReqSuffix, qfd, id+wakecmd.TypingSuffix) == nil {
		finishWakeCommand(cfg, qfd, id, wakecmd.Refused, why)
	}
}

// finishWakeCommand records the final state of claimed request id and then
// removes the claim. When the state cannot be recorded the claim stays: a
// command that may have been typed is never taken again.
func finishWakeCommand(cfg *wakeConfig, qfd int, id, state, reason string) {
	if err := markWakeCommand(qfd, id, state, reason); err != nil {
		_ = writeWakeDiagnostic(cfg, "amq wake: record the outcome of owner command %s: %v\n", id, err)
		return
	}
	_ = wakeUnlinkAt(qfd, id+wakecmd.TypingSuffix, 0)
}

// markWakeCommandsQueued records Queued for each of reqs.
func markWakeCommandsQueued(qfd int, reqs []wakecmd.Request, why string) {
	for _, req := range reqs {
		_ = markWakeCommand(qfd, req.ID, wakecmd.Queued, why)
	}
}

// markWakeCommand writes the outcome of request id, unless it is already
// that outcome.
func markWakeCommand(qfd int, id, state, reason string) error {
	line := wakecmd.OutcomeLine(state, reason)
	name := id + wakecmd.OutSuffix
	if raw, _, exists, err := readWakeRepairMetadataAt(qfd, name, "owner command outcome", name, len(line)); exists && err == nil && string(raw) == string(line) {
		return nil
	}
	temp, err := writeWakeReadyTempAt(qfd, "wake-command", line, 0o600)
	if err != nil {
		return err
	}
	if err := wakeRenameAt(qfd, temp, qfd, name); err != nil {
		_ = wakeUnlinkAt(qfd, temp, 0)
		return err
	}
	return nil
}
