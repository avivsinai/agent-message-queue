package cli

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const (
	threadOpenSchema        = "amq/thread-open/v1"
	threadOpenDefaultMaxAge = 14 * 24 * time.Hour

	obligationPending      = "pending"
	obligationAcknowledged = "acknowledged"
)

// openerKinds are the kinds that open an obligation for each recipient.
var openerKinds = []string{format.KindReviewRequest, format.KindQuestion, format.KindTodo}

type threadOpenResult struct {
	Schema        string                 `json:"schema"`
	GeneratedAt   string                 `json:"generated_at"`
	BaseRoot      string                 `json:"base_root"`
	Sessions      []string               `json:"sessions"`
	MaxAgeSeconds int64                  `json:"max_age_seconds"`
	Obligations   []threadOpenObligation `json:"obligations"`
	Expired       []threadOpenExpired    `json:"expired"`
	PrunedFiles   []threadOpenPruned     `json:"pruned_files"`
	Skipped       []threadOpenSkipped    `json:"skipped"`
}

type threadOpenObligation struct {
	Key          string              `json:"key"`
	Session      string              `json:"session"`
	Thread       string              `json:"thread"`
	Opener       threadOpenOpener    `json:"opener"`
	OwedBy       string              `json:"owed_by"`
	OwedByKind   string              `json:"owed_by_kind"`
	OwedTo       string              `json:"owed_to"`
	State        string              `json:"state"`
	Expects      []string            `json:"expects"`
	LastActivity *threadOpenActivity `json:"last_activity,omitempty"`
	AgeSeconds   int64               `json:"age_seconds"`
}

type threadOpenOpener struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	From        string   `json:"from"`
	FromProject string   `json:"from_project"`
	ReplyTo     string   `json:"reply_to"`
	To          []string `json:"to"`
	Subject     string   `json:"subject"`
	Created     string   `json:"created"`
	Priority    string   `json:"priority"`
	Labels      []string `json:"labels"`
}

type threadOpenActivity struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	From    string `json:"from"`
	Created string `json:"created"`
}

type threadOpenExpired struct {
	Session string `json:"session"`
	OwedBy  string `json:"owed_by"`
	Count   int    `json:"count"`
}

type threadOpenPruned struct {
	Session string `json:"session"`
	Count   int    `json:"count"`
}

type threadOpenSkipped struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// obligation is the open debt of one recipient (owner) to the sender
// (requester) of one opener message.
type obligation struct {
	key           string
	session       string // recipient session
	senderSession string // session the opener was sent from
	opener        *thread.Entry
	owner         string
	requester     string
	state         string
	closed        bool
	last          *thread.Entry
}

type threadOpenFilter struct {
	owedBy, owedTo, kinds []string
}

func (f threadOpenFilter) match(kind, owner, requester string) bool {
	return (len(f.kinds) == 0 || slices.Contains(f.kinds, kind)) &&
		(len(f.owedBy) == 0 || slices.Contains(f.owedBy, owner)) &&
		(len(f.owedTo) == 0 || slices.Contains(f.owedTo, requester))
}

func runThreadOpen(args []string) error {
	fs := flag.NewFlagSet("thread open", flag.ContinueOnError)
	common := addCommonFlags(fs)
	allSessions := fs.Bool("all-sessions", false, "Scan every canonical session under the base root")
	owedByFlag := fs.String("owed-by", "", "Comma-separated handles that owe a reply")
	owedToFlag := fs.String("owed-to", "", "Comma-separated handles that are owed a reply")
	kindFlag := fs.String("kind", "", "Comma-separated opener kinds: review_request, question, todo")
	maxAgeFlag := fs.String("max-age", "14d", "Ignore openers older than this (e.g. 14d, 36h; 0 = read every file)")
	limitFlag := fs.Int("limit", 0, "Limit number of obligations, oldest first (0 = no limit)")
	schemaFlag := fs.Int("json-schema", 1, "JSON schema version: 1 (requires --json)")

	usage := usageWithFlags(fs, "amq thread open [--all-sessions] [--owed-by <handles>] [--owed-to <handles>] [--kind <kinds>] [--max-age <dur>] [options]",
		"List open review requests, questions, and todos per thread.",
		"An obligation closes when its owner replies with the expected kind or a closing label.",
		"Read-only: it never moves a message, writes a receipt, or reads a body.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if flagWasVisited(fs, "json-schema") && !common.JSON {
		return UsageError("--json-schema requires --json")
	}
	if *schemaFlag != 1 {
		return UsageError("--json-schema must be 1")
	}
	if *limitFlag < 0 {
		return UsageError("--limit must be >= 0")
	}
	maxAge, err := parseMaxAge(*maxAgeFlag)
	if err != nil {
		return UsageError("--max-age: %v", err)
	}
	var filter threadOpenFilter
	if filter.owedBy, err = parseHandles(*owedByFlag); err != nil {
		return UsageError("--owed-by: %v", err)
	}
	if filter.owedTo, err = parseHandles(*owedToFlag); err != nil {
		return UsageError("--owed-to: %v", err)
	}
	for _, kind := range splitList(*kindFlag) {
		if !slices.Contains(openerKinds, kind) {
			return UsageError("--kind must be one of: %s", strings.Join(openerKinds, ", "))
		}
		filter.kinds = append(filter.kinds, kind)
	}

	root := absPath(resolveRoot(common.Root))
	if !dirExists(root) {
		return NotFoundError("root not found at %s", root)
	}
	// Inspection path: a session pin mismatch is reported and the scan goes on.
	if mismatch, err := sessionPinMismatch(root); err != nil {
		_ = writeStderr("warning: %v\n", err)
	} else if mismatch != nil && !isPinnedBaseRoot(root) {
		_ = writeStderr("warning: %s\n", mismatch.Error())
	}

	baseRoot := absPath(baseRootOf(root))
	scanRoots := []thread.ScanRoot{{Session: sessionName(root), Root: root}}
	if *allSessions {
		if baseRoot, err = sessionBaseRoot(root); err != nil {
			return err
		}
		if scanRoots, err = canonicalSessionRoots(baseRoot); err != nil {
			return err
		}
	}

	now := time.Now().UTC()
	var cutoff time.Time
	if maxAge > 0 {
		cutoff = now.Add(-maxAge)
	}
	result := threadOpenResult{
		Schema:        threadOpenSchema,
		GeneratedAt:   now.Format(time.RFC3339),
		BaseRoot:      baseRoot,
		Sessions:      []string{},
		MaxAgeSeconds: int64(maxAge / time.Second),
		Obligations:   []threadOpenObligation{},
		Expired:       []threadOpenExpired{},
		PrunedFiles:   []threadOpenPruned{},
		Skipped:       []threadOpenSkipped{},
	}
	for _, scanRoot := range scanRoots {
		result.Sessions = append(result.Sessions, scanRoot.Session)
	}

	scan, err := thread.CollectAll(scanRoots, cutoff, func(path string, parseErr error) error {
		result.Skipped = append(result.Skipped, threadOpenSkipped{Path: path, Error: parseErr.Error()})
		return nil
	})
	if err != nil {
		return err
	}
	for _, session := range result.Sessions {
		if count := scan.Pruned[session]; count > 0 {
			result.PrunedFiles = append(result.PrunedFiles, threadOpenPruned{Session: session, Count: count})
		}
	}

	open, expired, skipped := computeObligations(scan.Entries, cutoff, filter)
	result.Skipped = append(result.Skipped, skipped...)
	result.Expired = append(result.Expired, expired...)
	for _, ob := range open {
		result.Obligations = append(result.Obligations, obligationJSON(ob, now))
	}
	if *limitFlag > 0 && len(result.Obligations) > *limitFlag {
		result.Obligations = result.Obligations[:*limitFlag]
	}

	if common.JSON {
		return writeJSON(os.Stdout, result)
	}
	return writeThreadOpenText(result)
}

// computeObligations replays messages in header created order, then id, and
// returns the obligations still open, oldest first; the expired openers; and
// the messages whose created stamp does not parse.
func computeObligations(entries []thread.Entry, cutoff time.Time, filter threadOpenFilter) ([]*obligation, []threadOpenExpired, []threadOpenSkipped) {
	byOpener := map[string][]*obligation{}
	byThread := map[string][]*obligation{}
	all := []*obligation{}
	expiredCount := map[[2]string]int{}
	skipped := []threadOpenSkipped{}

	for i := range entries {
		msg := &entries[i]
		if msg.RawTime.IsZero() {
			skipped = append(skipped, threadOpenSkipped{Path: msg.Occurrences[0].Path, Error: fmt.Sprintf("invalid created timestamp %q", msg.Created)})
			continue
		}
		sender := msg.From

		// 1. Link: by reference back to the opener, or by thread within the
		// same session when the message carries no reference.
		linked := []*obligation{}
		if len(msg.Refs) > 0 {
			for _, ref := range msg.Refs {
				for _, ob := range byOpener[ref] {
					if !ob.closed && (ob.owner == sender || ob.requester == sender) && !slices.Contains(linked, ob) {
						linked = append(linked, ob)
					}
				}
			}
		} else {
			for _, ob := range byThread[msg.Thread] {
				if ob.closed {
					continue
				}
				switch sender {
				case ob.owner:
					if sentFrom(msg, ob.session) {
						linked = append(linked, ob)
					}
				case ob.requester:
					if sentFrom(msg, ob.senderSession) {
						linked = append(linked, ob)
					}
				}
			}
		}

		// 2. Close, then 3. acknowledge.
		for _, ob := range linked {
			ob.last = msg
			if closesObligation(ob.opener.Kind, sender == ob.owner, msg) {
				ob.closed = true
				continue
			}
			if sender == ob.owner {
				ob.state = obligationAcknowledged
			}
		}

		// 4. Open one obligation per new recipient.
		if !slices.Contains(openerKinds, msg.Kind) {
			continue
		}
		senderSession := messageSenderSession(msg)
		expired := !cutoff.IsZero() && msg.RawTime.Before(cutoff)
		seen := map[string]bool{}
		for _, recipient := range msg.To {
			if recipient == sender || seen[recipient] {
				continue
			}
			seen[recipient] = true
			if len(msg.Refs) > 0 && continuesObligation(linked, msg.Kind, sender, recipient) {
				continue
			}
			session := recipientSession(msg, recipient, senderSession)
			if expired {
				if filter.match(msg.Kind, recipient, sender) {
					expiredCount[[2]string{session, recipient}]++
				}
				continue
			}
			ob := &obligation{
				key:           session + "/" + msg.ID + "/" + recipient,
				session:       session,
				senderSession: senderSession,
				opener:        msg,
				owner:         recipient,
				requester:     sender,
				state:         obligationPending,
			}
			byOpener[msg.ID] = append(byOpener[msg.ID], ob)
			byThread[msg.Thread] = append(byThread[msg.Thread], ob)
			all = append(all, ob)
		}
	}

	open := []*obligation{}
	for _, ob := range all {
		if !ob.closed && filter.match(ob.opener.Kind, ob.owner, ob.requester) {
			open = append(open, ob)
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i].opener.RawTime, open[j].opener.RawTime
		if !a.Equal(b) {
			return a.Before(b)
		}
		return open[i].key < open[j].key
	})

	expired := []threadOpenExpired{}
	for k, count := range expiredCount {
		expired = append(expired, threadOpenExpired{Session: k[0], OwedBy: k[1], Count: count})
	}
	sort.Slice(expired, func(i, j int) bool {
		if expired[i].Session != expired[j].Session {
			return expired[i].Session < expired[j].Session
		}
		return expired[i].OwedBy < expired[j].OwedBy
	})
	return open, expired, skipped
}

// continuesObligation reports whether an open linked obligation of the same
// kind already binds the same pair: a requester's nudge, or an owner's reply
// that kept the default same kind. Only a message with refs can continue an
// obligation: reply always carries refs, so a message without refs is a new
// request even when it shares the thread with an open one.
func continuesObligation(linked []*obligation, kind, sender, recipient string) bool {
	for _, ob := range linked {
		if ob.closed || ob.opener.Kind != kind {
			continue
		}
		if (ob.owner == recipient && ob.requester == sender) || (ob.owner == sender && ob.requester == recipient) {
			return true
		}
	}
	return false
}

// closesObligation applies the closer table: which kinds and labels from the
// owner or the requester close an opener of the given kind.
func closesObligation(openerKind string, fromOwner bool, msg *thread.Entry) bool {
	hasLabel := func(labels ...string) bool {
		for _, label := range msg.Labels {
			if slices.Contains(labels, label) {
				return true
			}
		}
		return false
	}
	if msg.Kind == format.KindDecision {
		return true
	}
	if !fromOwner {
		switch openerKind {
		case format.KindReviewRequest:
			return hasLabel("withdrawn", "done", "merged", "closed")
		default:
			return hasLabel("withdrawn", "done", "closed")
		}
	}
	switch openerKind {
	case format.KindReviewRequest:
		return msg.Kind == format.KindReviewResponse || hasLabel("done", "merged", "closed")
	case format.KindQuestion:
		return msg.Kind == format.KindAnswer || hasLabel("done", "closed")
	case format.KindTodo:
		return (msg.Kind == format.KindStatus || msg.Kind == format.KindTodo) && hasLabel("done", "merged", "closed", "wontfix")
	}
	return false
}

// expectedReplies lists the owner reply kinds that can close an opener.
func expectedReplies(openerKind string) []string {
	switch openerKind {
	case format.KindReviewRequest:
		return []string{format.KindReviewResponse, format.KindDecision}
	case format.KindQuestion:
		return []string{format.KindAnswer, format.KindDecision}
	default:
		return []string{format.KindStatus, format.KindTodo, format.KindDecision}
	}
}

// sentFrom reports whether the sender's outbox in session holds msg.
func sentFrom(msg *thread.Entry, session string) bool {
	for _, occ := range msg.Occurrences {
		if occ.Session == session && occ.Mailbox == msg.From && occ.Dir == fsq.MailboxOutboxSent {
			return true
		}
	}
	return false
}

// messageSenderSession is the session of the sender's outbox copy; else the
// session named by reply_to or by the sender's side of a cross-session thread
// id; else the session of the first copy seen.
func messageSenderSession(msg *thread.Entry) string {
	for _, occ := range msg.Occurrences {
		if occ.Mailbox == msg.From && occ.Dir == fsq.MailboxOutboxSent {
			return occ.Session
		}
	}
	if handle, session, ok := strings.Cut(msg.ReplyTo, "@"); ok && handle == msg.From && session != "" {
		return session
	}
	if session, ok := threadSessionFor(msg.Thread, msg.From); ok {
		return session
	}
	return msg.Occurrences[0].Session
}

// recipientSession is the session whose inbox for recipient holds msg; else
// the session named by the recipient's side of a cross-session thread id;
// else the sender's session.
func recipientSession(msg *thread.Entry, recipient, senderSession string) string {
	for _, occ := range msg.Occurrences {
		if occ.Mailbox == recipient && (occ.Dir == fsq.MailboxInboxNew || occ.Dir == fsq.MailboxInboxCur) {
			return occ.Session
		}
	}
	if session, ok := threadSessionFor(msg.Thread, recipient); ok {
		return session
	}
	return senderSession
}

// threadSessionFor reads the session of handle from a cross-session thread
// id of the form p2p/<session>:<a>__<session>:<b>.
func threadSessionFor(threadID, handle string) (string, bool) {
	pair, ok := strings.CutPrefix(threadID, "p2p/")
	if !ok {
		return "", false
	}
	left, right, ok := strings.Cut(pair, "__")
	if !ok {
		return "", false
	}
	for _, side := range []string{left, right} {
		session, name, ok := strings.Cut(side, ":")
		if ok && name == handle && session != "" && !strings.Contains(name, ":") {
			return session, true
		}
	}
	return "", false
}

func obligationJSON(ob *obligation, now time.Time) threadOpenObligation {
	opener := ob.opener
	owedByKind := "agent"
	if ob.owner == reservedHumanHandle {
		owedByKind = "human"
	}
	out := threadOpenObligation{
		Key:     ob.key,
		Session: ob.session,
		Thread:  opener.Thread,
		Opener: threadOpenOpener{
			ID:          opener.ID,
			Kind:        opener.Kind,
			From:        opener.From,
			FromProject: opener.FromProject,
			ReplyTo:     opener.ReplyTo,
			To:          nonNil(opener.To),
			Subject:     opener.Subject,
			Created:     opener.Created,
			Priority:    opener.Priority,
			Labels:      nonNil(opener.Labels),
		},
		OwedBy:     ob.owner,
		OwedByKind: owedByKind,
		OwedTo:     ob.requester,
		State:      ob.state,
		Expects:    expectedReplies(opener.Kind),
		AgeSeconds: int64(now.Sub(opener.RawTime) / time.Second),
	}
	if ob.last != nil {
		out.LastActivity = &threadOpenActivity{ID: ob.last.ID, Kind: ob.last.Kind, From: ob.last.From, Created: ob.last.Created}
	}
	return out
}

func writeThreadOpenText(result threadOpenResult) error {
	if len(result.Obligations) == 0 {
		if err := writeStdoutLine("No open obligations."); err != nil {
			return err
		}
	}
	for _, ob := range result.Obligations {
		subject := ob.Opener.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		age := (time.Duration(ob.AgeSeconds) * time.Second).Round(time.Minute)
		if err := writeStdout("%s  %s  %s owes %s  %s  %s  %s\n", ob.Session, age, ob.OwedBy, ob.OwedTo, ob.Opener.Kind, ob.State, subject); err != nil {
			return err
		}
	}
	for _, exp := range result.Expired {
		if err := writeStdout("expired: %s  %s owes %d older than the cutoff\n", exp.Session, exp.OwedBy, exp.Count); err != nil {
			return err
		}
	}
	for _, pruned := range result.PrunedFiles {
		if err := writeStdout("pruned: %s  %d files older than the cutoff were not read\n", pruned.Session, pruned.Count); err != nil {
			return err
		}
	}
	for _, skip := range result.Skipped {
		if err := writeStderr("warning: skipping unreadable message %s: %s\n", skip.Path, skip.Error); err != nil {
			return err
		}
	}
	return nil
}

// canonicalSessionRoots lists the canonical sessions under base, sorted by
// name, as amq session list reports them.
func canonicalSessionRoots(base string) ([]thread.ScanRoot, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	roots := []thread.ScanRoot{}
	for _, entry := range entries {
		item, _, skip := classifySessionChild(base, entry.Name())
		if skip || item.Kind != sessionKindCanonical {
			continue
		}
		roots = append(roots, thread.ScanRoot{Session: item.Name, Root: item.Path})
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Session < roots[j].Session })
	return roots, nil
}

// parseMaxAge accepts a Go duration or a whole number of days ("14d"). Zero
// disables the cutoff.
func parseMaxAge(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "0" {
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", raw)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(raw); err != nil {
			return 0, err
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("must be >= 0")
	}
	return d, nil
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
