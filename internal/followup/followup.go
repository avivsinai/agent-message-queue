// Package followup keeps a sender's follow-up timers: a promise to tell the
// sender, in its own inbox, when one recipient has not replied to a request
// by a due time. A timer is a file in the sender's mailbox; whoever next
// drains, reads, or rings for that mailbox promotes the due ones.
package followup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// Schema is the timer file schema this package writes and accepts.
const Schema = 1

// Label marks a follow-up notice.
const Label = "follow-up"

// QuarantineSuffix ends the name of a timer file that could not be parsed.
// Such a file never fires and is kept for the operator.
const QuarantineSuffix = ".bad"

const (
	timerSuffix = ".json"
	lockName    = ".lock"
)

// Timer is one armed follow-up, written once by the sender and never
// modified. Its notice is a pure function of the timer and the mailbox owner,
// so every promotion of it builds the same bytes.
type Timer struct {
	Schema        int       `json:"schema"`
	RequestID     string    `json:"request_id"`
	To            string    `json:"to"`
	TargetSession string    `json:"target_session,omitempty"`
	TargetProject string    `json:"target_project,omitempty"`
	Thread        string    `json:"thread"`
	Subject       string    `json:"subject,omitempty"`
	RequestKind   string    `json:"request_kind,omitempty"`
	Sent          time.Time `json:"sent"`
	Due           time.Time `json:"due"`
	NoticeID      string    `json:"notice_id"`
}

// New builds the timer for request id, sent at sent to one recipient, due
// after the given duration. The notice id is minted here, from the due time,
// so the notice is the same on every promotion.
func New(requestID, to, thread, subject, kind string, sent time.Time, after time.Duration) (Timer, error) {
	due := sent.Add(after).UTC()
	noticeID, err := format.NewMessageID(due)
	if err != nil {
		return Timer{}, err
	}
	return Timer{
		Schema:      Schema,
		RequestID:   requestID,
		To:          to,
		Thread:      thread,
		Subject:     subject,
		RequestKind: kind,
		Sent:        sent.UTC(),
		Due:         due,
		NoticeID:    noticeID,
	}, nil
}

func (t Timer) validate() error {
	switch {
	case t.Schema != Schema:
		return fmt.Errorf("schema %d, want %d", t.Schema, Schema)
	case fsq.ValidateMessageFilename(t.RequestID+".md") != nil:
		return fmt.Errorf("invalid request_id %q", t.RequestID)
	case fsq.ValidateHandle(t.To) != nil:
		return fmt.Errorf("invalid to %q", t.To)
	case fsq.ValidateMessageFilename(t.NoticeID+".md") != nil:
		return fmt.Errorf("invalid notice_id %q", t.NoticeID)
	case t.Thread == "":
		return errors.New("missing thread")
	case t.Due.IsZero():
		return errors.New("missing due")
	}
	return nil
}

func dir(me string) string {
	return fsq.MailboxRootRelativePath(me, fsq.MailboxFollowUps)
}

// Write arms t in me's mailbox: mode 0600 under a 0700 directory that is
// created on first use, through tmp, fsync, and rename.
func Write(root *fsq.DeliveryRoot, me string, t Timer) error {
	if err := t.validate(); err != nil {
		return fmt.Errorf("follow-up timer: %w", err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := root.EnsureAgentDir(me, fsq.MailboxFollowUps); err != nil {
		return err
	}
	_, err = root.WriteFileAtomic(dir(me), t.RequestID+timerSuffix, append(data, '\n'), 0o600)
	return err
}

// List returns me's armed timers, earliest due first. It never writes; a
// file that does not parse is skipped here and quarantined by Promote.
func List(root *fsq.DeliveryRoot, me string) ([]Timer, error) {
	timers, _, err := scan(root, me)
	return timers, err
}

// Notice is the message that tells me no reply arrived for t.
func Notice(t Timer, me string) format.Message {
	subject := "No reply from " + t.To
	if t.Subject != "" {
		subject += ": " + t.Subject
	}
	return format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       t.NoticeID,
			From:     me,
			To:       []string{me},
			Thread:   t.Thread,
			Subject:  subject,
			Created:  t.Due.UTC().Format(time.RFC3339Nano),
			Refs:     []string{t.RequestID},
			Priority: format.PriorityNormal,
			Kind:     format.KindStatus,
			Labels:   []string{Label},
			Context: map[string]any{"follow_up": map[string]any{
				"request_id": t.RequestID,
				"to":         t.To,
				"sent":       t.Sent.UTC().Format(time.RFC3339Nano),
				"due":        t.Due.UTC().Format(time.RFC3339Nano),
			}},
		},
		Body: "No reply to this request arrived before its follow-up time.\n\namq trace " + t.RequestID + " --follow\n",
	}
}

// Promote delivers the notice of every timer in me's mailbox that is due at
// now and unanswered, and drops every due timer it settles.
//
// A request is answered by a message in me's inbox (new or cur) from the
// recipient, compared by handle, whose refs name the request: amq reply
// always adds the replied id to refs. Thread is never used, because the
// default thread is shared by every message between the two agents. A
// message in me's outbox/sent whose refs name the request and whose labels
// close it for the requester (withdrawn, done, closed) also settles it.
//
// Promotion holds a non-blocking lock on follow-ups/.lock; a busy lock means
// another promoter is working and this one returns. Per due timer, in order:
//
//	settled                       -> unlink the timer
//	notice already in inbox       -> unlink the timer
//	otherwise                     -> deliver the notice, then unlink the timer
//
// A crash after delivery and before the unlink leaves a timer whose notice
// is in the inbox; the next pass unlinks it without a second notice, because
// the notice id was minted when the timer was written. A failed delivery or
// inbox scan keeps the timer for the next pass. A timer never fires before
// its due time. A timer file that does not parse is renamed with
// QuarantineSuffix and reported in the returned error; it never fires.
func Promote(root *fsq.DeliveryRoot, me string, now time.Time) error {
	timers, broken, err := scan(root, me)
	if err != nil || (broken == 0 && !anyDue(timers, now)) {
		return err
	}
	if !lock.AdvisoryLockAvailable() {
		return errors.New("follow-ups need advisory file locks, which this platform lacks")
	}
	file, err := root.OpenLockFile(dir(me), lockName, 0o600)
	if err != nil {
		return fmt.Errorf("follow-up lock: %w", err)
	}
	defer func() { _ = file.Close() }()
	held, err := lock.TryLock(file)
	if err != nil {
		return fmt.Errorf("follow-up lock: %w", err)
	}
	if !held {
		return nil
	}
	defer func() { _ = lock.Unlock(file) }()

	var errs []error
	timers, err = scanQuarantining(root, me, &errs)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var due []Timer
	for _, t := range timers {
		if !t.Due.After(now) {
			due = append(due, t)
		}
	}
	if len(due) == 0 {
		return errors.Join(errs...)
	}
	box, err := readMailbox(root, me, due)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("follow-up: scan mailbox: %w", err))...)
	}
	for _, t := range due {
		if err := promoteOne(root, me, t, box); err != nil {
			errs = append(errs, fmt.Errorf("follow-up %s: %w", t.RequestID, err))
		}
	}
	return errors.Join(errs...)
}

func anyDue(timers []Timer, now time.Time) bool {
	return slices.ContainsFunc(timers, func(t Timer) bool { return !t.Due.After(now) })
}

func promoteOne(root *fsq.DeliveryRoot, me string, t Timer, box mailbox) error {
	if !box.settles(t) && !box.noticeIDs[t.NoticeID] {
		data, err := Notice(t, me).Marshal()
		if err != nil {
			return err
		}
		if _, err := fsq.DeliverToInbox(root, me, t.NoticeID+".md", data); err != nil {
			var committed *fsq.CommittedDurabilityError
			if !errors.As(err, &committed) {
				return err
			}
		}
	}
	if err := root.Remove(filepath.Join(dir(me), t.RequestID+timerSuffix)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return root.SyncDir(dir(me))
}

// mailbox is what one promotion pass read of me's mailbox.
type mailbox struct {
	inbox     []format.Header
	sent      []format.Header
	noticeIDs map[string]bool
}

func (m mailbox) settles(t Timer) bool {
	for _, h := range m.inbox {
		if h.ID != t.RequestID && sameHandle(h.From, t.To) && slices.Contains(h.Refs, t.RequestID) {
			return true
		}
	}
	for _, h := range m.sent {
		if h.ID != t.RequestID && slices.Contains(h.Refs, t.RequestID) && thread.RequesterCloses(t.RequestKind, h.Labels) {
			return true
		}
	}
	return false
}

// sameHandle compares a header sender with a local handle; a bridged sender
// carries a host prefix, host/handle.
func sameHandle(from, handle string) bool {
	if from == handle {
		return true
	}
	_, h, ok := strings.Cut(from, "/")
	return ok && h == handle
}

// readMailbox reads the headers that can settle the due timers. A file whose
// name stamp is older than the earliest request cannot reply to it and is
// not opened; a file that does not parse is no reply. A directory that
// cannot be listed fails the pass.
func readMailbox(root *fsq.DeliveryRoot, me string, due []Timer) (mailbox, error) {
	var cutoff time.Time
	for i, t := range due {
		stamp, ok := format.IDTime(t.RequestID)
		if !ok {
			cutoff = time.Time{}
			break
		}
		if i == 0 || stamp.Before(cutoff) {
			cutoff = stamp
		}
	}
	notices := map[string]bool{}
	for _, t := range due {
		notices[t.NoticeID+".md"] = true
	}
	box := mailbox{noticeIDs: map[string]bool{}}
	for _, leaf := range []fsq.MailboxLeaf{fsq.MailboxInboxNew, fsq.MailboxInboxCur, fsq.MailboxOutboxSent} {
		d := fsq.MailboxRootRelativePath(me, leaf)
		entries, err := root.ReadDir(d)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return mailbox{}, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
				continue
			}
			if notices[name] && leaf != fsq.MailboxOutboxSent {
				box.noticeIDs[strings.TrimSuffix(name, ".md")] = true
				continue
			}
			if stamp, ok := format.IDTime(name); ok && !cutoff.IsZero() && stamp.Before(cutoff) {
				continue
			}
			h, err := format.ReadHeaderFileRoot(root, filepath.Join(d, name))
			if err != nil {
				continue
			}
			if leaf == fsq.MailboxOutboxSent {
				box.sent = append(box.sent, h)
			} else {
				box.inbox = append(box.inbox, h)
			}
		}
	}
	return box, nil
}

// scan reads every timer file. broken counts files that do not parse.
func scan(root *fsq.DeliveryRoot, me string) (timers []Timer, broken int, err error) {
	entries, err := root.ReadDir(dir(me))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("read follow-ups: %w", err)
	}
	for _, entry := range entries {
		if !isTimerEntry(entry) {
			continue
		}
		t, err := readTimer(root, me, entry.Name())
		if err != nil {
			if !os.IsNotExist(err) {
				broken++
			}
			continue
		}
		timers = append(timers, t)
	}
	sortByDue(timers)
	return timers, broken, nil
}

// scanQuarantining is scan under the lock: a file that does not parse is
// renamed aside once and reported in errs.
func scanQuarantining(root *fsq.DeliveryRoot, me string, errs *[]error) ([]Timer, error) {
	entries, err := root.ReadDir(dir(me))
	if err != nil {
		return nil, fmt.Errorf("read follow-ups: %w", err)
	}
	var timers []Timer
	for _, entry := range entries {
		if !isTimerEntry(entry) {
			continue
		}
		name := entry.Name()
		t, err := readTimer(root, me, name)
		var parseErr *timerParseError
		switch {
		case err == nil:
			timers = append(timers, t)
		case errors.As(err, &parseErr):
			path := filepath.Join(dir(me), name)
			if mvErr := root.RenameNoReplace(path, path+QuarantineSuffix); mvErr != nil {
				err = errors.Join(err, fmt.Errorf("quarantine: %w", mvErr))
			} else {
				err = fmt.Errorf("%w (moved to %s%s)", err, name, QuarantineSuffix)
			}
			*errs = append(*errs, err)
		case !os.IsNotExist(err):
			*errs = append(*errs, fmt.Errorf("read follow-up %s: %w", name, err))
		}
	}
	sortByDue(timers)
	return timers, nil
}

func sortByDue(timers []Timer) {
	sort.SliceStable(timers, func(i, j int) bool { return timers[i].Due.Before(timers[j].Due) })
}

func isTimerEntry(entry os.DirEntry) bool {
	name := entry.Name()
	return entry.Type().IsRegular() && !strings.HasPrefix(name, ".") && strings.HasSuffix(name, timerSuffix)
}

type timerParseError struct {
	name string
	err  error
}

func (e *timerParseError) Error() string {
	return fmt.Sprintf("invalid follow-up timer %s: %v", e.name, e.err)
}

func readTimer(root *fsq.DeliveryRoot, me, name string) (Timer, error) {
	data, err := root.ReadRegularNoFollow(filepath.Join(dir(me), name))
	if err != nil {
		return Timer{}, err
	}
	var t Timer
	if err := json.Unmarshal(data, &t); err != nil {
		return Timer{}, &timerParseError{name: name, err: err}
	}
	if err := t.validate(); err != nil {
		return Timer{}, &timerParseError{name: name, err: err}
	}
	if t.RequestID+timerSuffix != name {
		return Timer{}, &timerParseError{name: name, err: fmt.Errorf("names request %s", t.RequestID)}
	}
	return t, nil
}
