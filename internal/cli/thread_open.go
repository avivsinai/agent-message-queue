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
	threadOpenSchema = "amq/thread-open/v1"

	obligationPending      = "pending"
	obligationAcknowledged = "acknowledged"

	// A continuation whose direct target is not in the scan is looked up by
	// file name: at most this many missing hops per message and lookups per
	// run.
	threadOpenMaxHops    = 8
	threadOpenMaxLookups = 512
)

// openerKinds are the kinds that open an obligation for each recipient.
var openerKinds = []string{format.KindReviewRequest, format.KindQuestion, format.KindTodo}

// closingLabels make a message terminal: it never opens an obligation.
var closingLabels = []string{"done", "merged", "closed", "wontfix", "withdrawn"}

type threadOpenResult struct {
	Schema      string                 `json:"schema"`
	GeneratedAt string                 `json:"generated_at"`
	BaseRoot    string                 `json:"base_root"`
	Sessions    []string               `json:"sessions"`
	Obligations []threadOpenObligation `json:"obligations"`
	Coverage    threadOpenCoverage     `json:"coverage"`
}

type threadOpenObligation struct {
	Key                   string                 `json:"key"`
	Thread                string                 `json:"thread"`
	Opener                threadOpenOpener       `json:"opener"`
	OwedBy                participant            `json:"owed_by"`
	OwedByKind            string                 `json:"owed_by_kind"`
	OwedTo                participant            `json:"owed_to"`
	ReplyDestination      *threadOpenDestination `json:"reply_destination"`
	ReplyDestinationError string                 `json:"reply_destination_error,omitempty"`
	State                 string                 `json:"state"`
	Completion            threadOpenRecipe       `json:"completion"`
	Acknowledge           threadOpenRecipe       `json:"acknowledge"`
	LastActivity          *threadOpenActivity    `json:"last_activity,omitempty"`
	AgeSeconds            int64                  `json:"age_seconds"`
}

type threadOpenOpener struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	From         string   `json:"from"`
	To           []string `json:"to"`
	ReplyTo      string   `json:"reply_to"`
	ReplyProject string   `json:"reply_project"`
	FromProject  string   `json:"from_project"`
	Subject      string   `json:"subject"`
	Created      string   `json:"created"`
	Priority     string   `json:"priority"`
	Labels       []string `json:"labels"`
}

// participant is one party to an obligation. Project is empty for the
// project under scan; Session is empty for a base root. Handles alone never
// identify a participant.
type participant struct {
	Project string `json:"project"`
	Session string `json:"session"`
	Handle  string `json:"handle"`
}

type threadOpenDestination struct {
	Handle  string `json:"handle"`
	Session string `json:"session"`
	Project string `json:"project"`
	Source  string `json:"source"`
}

type threadOpenRecipe struct {
	Kind   string   `json:"kind"`
	Labels []string `json:"labels"`
}

type threadOpenActivity struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	From    string `json:"from"`
	Created string `json:"created"`
}

type threadOpenCoverage struct {
	MaxAgeSeconds     int64                  `json:"max_age_seconds"`
	AssessedSince     string                 `json:"assessed_since,omitempty"`
	PrunedFiles       []threadOpenCount      `json:"pruned_files"`
	Expired           []threadOpenExpired    `json:"expired"`
	Skipped           []threadOpenSkipped    `json:"skipped"`
	UnresolvedLineage []threadOpenUnresolved `json:"unresolved_lineage"`
	Ambiguous         []threadOpenAmbiguous  `json:"ambiguous"`
	OutboundRouted    []threadOpenCount      `json:"outbound_routed"`
	TruncatedByLimit  threadOpenTruncated    `json:"truncated_by_limit"`
}

type threadOpenCount struct {
	Session string `json:"session"`
	Count   int    `json:"count"`
}

type threadOpenExpired struct {
	Session string `json:"session"`
	OwedBy  string `json:"owed_by"`
	Count   int    `json:"count"`
}

type threadOpenSkipped struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type threadOpenUnresolved struct {
	ID         string `json:"id"`
	Session    string `json:"session"`
	From       string `json:"from"`
	Kind       string `json:"kind"`
	MissingRef string `json:"missing_ref"`
}

type threadOpenAmbiguous struct {
	ID         string   `json:"id"`
	Session    string   `json:"session"`
	From       string   `json:"from"`
	Reason     string   `json:"reason"`
	Candidates []string `json:"candidates"`
}

type threadOpenTruncated struct {
	Limit   int `json:"limit"`
	Omitted int `json:"omitted"`
}

type threadOpenFilter struct {
	owedBy, owedTo, kinds []string
}

func (f threadOpenFilter) match(ob *obligation) bool {
	return (len(f.kinds) == 0 || slices.Contains(f.kinds, ob.opener.Kind)) &&
		(len(f.owedBy) == 0 || slices.Contains(f.owedBy, ob.owner.Handle)) &&
		(len(f.owedTo) == 0 || slices.Contains(f.owedTo, ob.requester.Handle))
}

func runThreadOpen(args []string) error {
	fs := flag.NewFlagSet("thread open", flag.ContinueOnError)
	common := addCommonFlags(fs)
	allSessions := fs.Bool("all-sessions", false, "Scan every canonical session under the base root")
	owedByFlag := fs.String("owed-by", "", "Comma-separated handles that owe a reply")
	owedToFlag := fs.String("owed-to", "", "Comma-separated handles that are owed a reply")
	kindFlag := fs.String("kind", "", "Comma-separated opener kinds: review_request, question, todo")
	maxAgeFlag := fs.String("max-age", "14d", "Ignore openers older than this (e.g. 14d, 36h; 0 = read every file)")
	limitFlag := fs.Int("limit", 0, "List at most N obligations, oldest first, after the whole window is reduced (0 = no limit)")
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
	session := ""
	if root != baseRoot {
		session = sessionName(root)
	}
	scanRoots := []thread.ScanRoot{{Session: session, Root: root}}
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
		Schema:      threadOpenSchema,
		GeneratedAt: now.Format(time.RFC3339),
		BaseRoot:    baseRoot,
		Sessions:    []string{},
		Obligations: []threadOpenObligation{},
		Coverage: threadOpenCoverage{
			MaxAgeSeconds: int64(maxAge / time.Second),
			PrunedFiles:   []threadOpenCount{},
			Skipped:       []threadOpenSkipped{},
		},
	}
	if !cutoff.IsZero() {
		result.Coverage.AssessedSince = cutoff.Format(time.RFC3339)
	}
	for _, scanRoot := range scanRoots {
		result.Sessions = append(result.Sessions, scanRoot.Session)
	}

	scan, err := thread.CollectAll(scanRoots, cutoff, func(path string, parseErr error) error {
		result.Coverage.Skipped = append(result.Coverage.Skipped, threadOpenSkipped{Path: path, Error: parseErr.Error()})
		return nil
	})
	if err != nil {
		return err
	}
	result.Coverage.PrunedFiles = countsBySession(result.Sessions, scan.Pruned)

	red := newObligationReducer(scanRoots, scan.Entries, cutoff)
	red.run()
	result.Coverage.Skipped = append(result.Coverage.Skipped, red.skipped...)
	result.Coverage.UnresolvedLineage = red.unresolved
	result.Coverage.Ambiguous = red.ambiguous
	result.Coverage.OutboundRouted = countsBySession(result.Sessions, red.outbound)

	open, expired := red.report(filter)
	result.Coverage.Expired = expired
	result.Coverage.TruncatedByLimit.Limit = *limitFlag
	if *limitFlag > 0 && len(open) > *limitFlag {
		result.Coverage.TruncatedByLimit.Omitted = len(open) - *limitFlag
		open = open[:*limitFlag]
	}
	for _, ob := range open {
		result.Obligations = append(result.Obligations, obligationJSON(ob, now))
	}

	if common.JSON {
		return writeJSON(os.Stdout, result)
	}
	return writeThreadOpenText(result)
}

// obligation is the open debt of one recipient (owner) to the sender
// (requester) of one opener message.
type obligation struct {
	key       string
	opener    *thread.Entry
	owner     participant
	requester participant
	state     string
	closed    bool
	expired   bool
	last      *thread.Entry
}

// record is one message with its resolved sender and the obligations whose
// lineage it belongs to.
type record struct {
	entry      *thread.Entry
	sender     participant
	lineage    []*obligation
	missingRef string // set when the lineage of the message cannot be resolved
	hops       int    // records read by name on the way to this one
	waiting    bool   // waits for its direct target
	done       bool
}

// obligationReducer applies the thread open rules. Phase 1
// (newObligationReducer) indexes every message by id and resolves its
// sender; phase 2 (run) reduces over the reference graph.
type obligationReducer struct {
	roots       []thread.ScanRoot
	cutoff      time.Time
	records     map[string]*record
	order       []*record
	waiting     map[string][]*record // direct target id -> messages waiting for it
	obligations []*obligation
	byThread    map[string][]*obligation
	lookups     int
	skipped     []threadOpenSkipped
	unresolved  []threadOpenUnresolved
	ambiguous   []threadOpenAmbiguous
	outbound    map[string]int
}

func newObligationReducer(roots []thread.ScanRoot, entries []thread.Entry, cutoff time.Time) *obligationReducer {
	red := &obligationReducer{
		roots:      roots,
		cutoff:     cutoff,
		records:    map[string]*record{},
		byThread:   map[string][]*obligation{},
		waiting:    map[string][]*record{},
		skipped:    []threadOpenSkipped{},
		unresolved: []threadOpenUnresolved{},
		ambiguous:  []threadOpenAmbiguous{},
		outbound:   map[string]int{},
	}
	for i := range entries {
		red.order = append(red.order, red.add(&entries[i]))
	}
	return red
}

func (red *obligationReducer) add(entry *thread.Entry) *record {
	rec := &record{entry: entry, sender: senderOf(entry)}
	red.records[entry.ID] = rec
	return rec
}

// run reduces in two passes, so the result depends on neither file order
// nor the created time of any message with refs.
//
// First every missing direct target is read by name, so the set of
// messages is fixed before reduction starts.
//
// Pass 1 visits the messages without refs in created order, then id. An
// opener opens its obligations; any other message is matched against the
// obligations opened by earlier pass 1 openers and not closed by an earlier
// pass 1 message. Replies are not consulted.
//
// Pass 2 visits the messages with refs. Each waits until its direct target
// has settled and then acts on the target's lineage. Close and acknowledge
// are sticky and a message's lineage depends only on its settled target,
// never on its siblings, so the visiting order does not matter. A message
// still waiting at the end is on a reference cycle and is unresolved.
func (red *obligationReducer) run() {
	all := slices.Clone(red.order)
	for i := 0; i < len(all); i++ {
		if refs := all[i].entry.Refs; len(refs) > 0 {
			if found := red.lookup(refs[len(refs)-1], all[i]); found != nil {
				all = append(all, found)
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i].entry, all[j].entry
		if !a.RawTime.Equal(b.RawTime) {
			return a.RawTime.Before(b.RawTime)
		}
		return a.ID < b.ID
	})
	for _, rec := range all {
		if len(rec.entry.Refs) == 0 {
			red.visit(rec)
		}
	}
	for _, rec := range all {
		if len(rec.entry.Refs) > 0 {
			red.visit(rec)
		}
	}
	ids := make([]string, 0, len(red.waiting))
	for id := range red.waiting {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, rec := range red.waiting[id] {
			if !rec.done {
				red.markUnresolved(rec, id)
			}
		}
	}
}

func (red *obligationReducer) visit(rec *record) {
	if rec.done || rec.waiting {
		return
	}
	msg := rec.entry
	if msg.RawTime.IsZero() {
		red.skipped = append(red.skipped, threadOpenSkipped{Path: msg.Occurrences[0].Path, Error: fmt.Sprintf("invalid created timestamp %q", msg.Created)})
		red.finish(rec, nil)
		rec.missingRef = msg.ID
		red.release(rec)
		return
	}
	if len(msg.Refs) == 0 {
		var acted []*obligation
		if !slices.Contains(openerKinds, msg.Kind) || isTerminal(msg) {
			acted = red.actWithoutRefs(rec)
		}
		red.finish(rec, acted)
		red.release(rec)
		return
	}
	// The last ref names the direct parent; earlier refs are inherited
	// context and give no authority.
	targetID := msg.Refs[len(msg.Refs)-1]
	target := red.records[targetID]
	switch {
	case target == nil:
		red.markUnresolved(rec, targetID)
		return
	case !target.done:
		rec.waiting = true
		red.waiting[target.entry.ID] = append(red.waiting[target.entry.ID], rec)
		return
	case target.missingRef != "":
		red.markUnresolved(rec, target.missingRef)
		return
	}

	var acted []*obligation
	for _, ob := range target.lineage {
		owner, requester := roleOf(ob, rec.sender, true)
		if !owner && !requester {
			continue
		}
		if !slices.Contains(msg.To, otherParty(ob, owner).Handle) {
			// The owner handing the work to someone else acknowledges it.
			if owner {
				acknowledge(ob, msg)
			}
			continue
		}
		act(ob, owner, msg)
		acted = append(acted, ob)
	}
	continuation := msg.Kind == defaultReplyKind(target.entry.Kind)
	if opened := red.open(rec, acted, continuation); len(opened) > 0 {
		rec.lineage = opened
	} else {
		rec.lineage = acted
	}
	rec.done = true
	red.release(rec)
}

// finish opens the obligations of a message without a direct target and
// records its lineage.
func (red *obligationReducer) finish(rec *record, acted []*obligation) {
	if opened := red.open(rec, acted, false); len(opened) > 0 {
		rec.lineage = opened
	} else {
		rec.lineage = acted
	}
	rec.done = true
}

// release runs the messages that waited for rec as their direct target.
func (red *obligationReducer) release(rec *record) {
	waiters := red.waiting[rec.entry.ID]
	delete(red.waiting, rec.entry.ID)
	for _, waiter := range waiters {
		waiter.waiting = false
		red.visit(waiter)
	}
}

func (red *obligationReducer) markUnresolved(rec *record, missing string) {
	rec.missingRef = missing
	rec.waiting = false
	rec.done = true
	red.unresolved = append(red.unresolved, threadOpenUnresolved{
		ID: rec.entry.ID, Session: rec.sender.Session, From: rec.entry.From, Kind: rec.entry.Kind, MissingRef: missing,
	})
	red.release(rec)
}

// lookup reads the header of a direct target the scan did not collect, by
// name, and returns the new record; nil when it is collected already or not
// found. A chain of records read by name is at most threadOpenMaxHops long.
func (red *obligationReducer) lookup(id string, from *record) *record {
	if _, ok := red.records[id]; ok {
		return nil
	}
	if from.hops >= threadOpenMaxHops || red.lookups >= threadOpenMaxLookups {
		return nil
	}
	red.lookups++
	agents := append([]string{from.entry.From}, from.entry.To...)
	entry, ok := thread.ReadByID(red.roots, agents, id)
	if !ok {
		return nil
	}
	rec := red.add(&entry)
	rec.hops = from.hops + 1
	return rec
}

// actWithoutRefs links a message without refs to the one open obligation in
// its thread between the same two exact participants.
func (red *obligationReducer) actWithoutRefs(rec *record) []*obligation {
	msg := rec.entry
	recipients := []participant{}
	unresolved := false
	for _, handle := range msg.To {
		if p, ok := recipientOf(rec, handle); ok {
			recipients = append(recipients, p)
		} else {
			unresolved = true
		}
	}
	candidates := []*obligation{}
	for _, ob := range red.byThread[msg.Thread] {
		if ob.closed || !ob.opener.RawTime.Before(msg.RawTime) {
			continue
		}
		owner, requester := roleOf(ob, rec.sender, false)
		if !owner && !requester {
			continue
		}
		if unresolved || slices.Contains(recipients, otherParty(ob, owner)) {
			candidates = append(candidates, ob)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if unresolved || len(candidates) > 1 {
		reason := fmt.Sprintf("%d candidates", len(candidates))
		if unresolved {
			reason = "recipient unresolved"
		}
		keys := []string{}
		for _, ob := range candidates {
			keys = append(keys, ob.key)
		}
		red.ambiguous = append(red.ambiguous, threadOpenAmbiguous{
			ID: msg.ID, Session: rec.sender.Session, From: msg.From, Reason: reason, Candidates: keys,
		})
		return nil
	}
	owner, _ := roleOf(candidates[0], rec.sender, false)
	act(candidates[0], owner, msg)
	return candidates
}

// open creates one obligation per resolved recipient of an opener. A
// terminal message opens nothing, and a continuation (the kind reply would
// default to) opens nothing for the other participant of an obligation it
// acts on.
func (red *obligationReducer) open(rec *record, acted []*obligation, continuation bool) []*obligation {
	msg := rec.entry
	if !slices.Contains(openerKinds, msg.Kind) || isTerminal(msg) {
		return nil
	}
	opened := []*obligation{}
	routed := false
	for _, handle := range slices.Compact(slices.Sorted(slices.Values(msg.To))) {
		if continuation && slices.ContainsFunc(acted, func(ob *obligation) bool {
			return otherParty(ob, rec.sender != ob.requester).Handle == handle
		}) {
			continue
		}
		owner, ok := recipientOf(rec, handle)
		if !ok {
			routed = true
			continue
		}
		if owner == rec.sender {
			continue
		}
		session := owner.Session
		if session == "" {
			session = "."
		}
		ob := &obligation{
			key:       session + "/" + msg.ID + "/" + owner.Handle,
			opener:    msg,
			owner:     owner,
			requester: rec.sender,
			state:     obligationPending,
			expired:   !red.cutoff.IsZero() && msg.RawTime.Before(red.cutoff),
		}
		opened = append(opened, ob)
		red.obligations = append(red.obligations, ob)
		red.byThread[msg.Thread] = append(red.byThread[msg.Thread], ob)
	}
	if routed {
		red.outbound[rec.sender.Session]++
	}
	return opened
}

// report returns the obligations still open, oldest opener first, and the
// expired openers counted per owner.
func (red *obligationReducer) report(filter threadOpenFilter) ([]*obligation, []threadOpenExpired) {
	open := []*obligation{}
	expiredCount := map[[2]string]int{}
	for _, ob := range red.obligations {
		if !filter.match(ob) {
			continue
		}
		if ob.expired {
			expiredCount[[2]string{ob.owner.Session, ob.owner.Handle}]++
			continue
		}
		if !ob.closed {
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
	return open, expired
}

// roleOf reports whether sender is the owner or the requester of ob. Through
// an exact reference, the reserved human of the project under scan acts as
// the owner from any of its sessions.
func roleOf(ob *obligation, sender participant, byReference bool) (owner, requester bool) {
	switch {
	case sender == ob.owner:
		return true, false
	case sender == ob.requester:
		return false, true
	case byReference && sender.Handle == reservedHumanHandle && sender.Project == "" &&
		ob.owner.Handle == reservedHumanHandle && ob.owner.Project == "":
		return true, false
	}
	return false, false
}

// otherParty is the participant a message from the owner (or, when owner is
// false, from the requester) of ob must be addressed to.
func otherParty(ob *obligation, owner bool) participant {
	if owner {
		return ob.requester
	}
	return ob.owner
}

func act(ob *obligation, fromOwner bool, msg *thread.Entry) {
	touch(ob, msg)
	if ob.closed {
		return
	}
	if closesObligation(ob.opener.Kind, fromOwner, msg) {
		ob.closed = true
		return
	}
	if fromOwner {
		ob.state = obligationAcknowledged
	}
}

func acknowledge(ob *obligation, msg *thread.Entry) {
	touch(ob, msg)
	if !ob.closed {
		ob.state = obligationAcknowledged
	}
}

func touch(ob *obligation, msg *thread.Entry) {
	if ob.last == nil || ob.last.RawTime.Before(msg.RawTime) {
		ob.last = msg
	}
}

// senderOf resolves who sent msg: the sender's outbox copy wins; otherwise
// the routing fields; otherwise the session of the inbox copy, where a local
// message is delivered.
func senderOf(msg *thread.Entry) participant {
	for _, occ := range msg.Occurrences {
		if occ.Dir == fsq.MailboxOutboxSent {
			return participant{Session: occ.Session, Handle: occ.Agent}
		}
	}
	_, replySession, _ := strings.Cut(msg.ReplyTo, "@")
	if msg.ReplyProject != "" {
		return participant{Project: msg.ReplyProject, Session: replySession, Handle: msg.From}
	}
	if msg.ReplyTo != "" {
		return participant{Session: replySession, Handle: msg.From}
	}
	return participant{Session: msg.Occurrences[0].Session, Handle: msg.From}
}

// recipientOf resolves a recipient handle of a message: the session of its
// inbox copy; else the sender's session for a local message. A routed
// message with no inbox copy in scope has no target metadata, so its
// recipient is unresolved.
func recipientOf(rec *record, handle string) (participant, bool) {
	for _, occ := range rec.entry.Occurrences {
		if occ.Agent == handle && (occ.Dir == fsq.MailboxInboxNew || occ.Dir == fsq.MailboxInboxCur) {
			return participant{Session: occ.Session, Handle: handle}, true
		}
	}
	if rec.entry.ReplyTo == "" && rec.sender.Project == "" {
		return participant{Session: rec.sender.Session, Handle: handle}, true
	}
	return participant{}, false
}

// defaultReplyKind is the kind amq reply sends when none is named.
func defaultReplyKind(kind string) string {
	switch kind {
	case format.KindReviewRequest:
		return format.KindReviewResponse
	case format.KindQuestion:
		return format.KindAnswer
	}
	return kind
}

func isTerminal(msg *thread.Entry) bool {
	return slices.ContainsFunc(msg.Labels, func(label string) bool { return slices.Contains(closingLabels, label) })
}

// closesObligation applies the closer table: which kinds and labels from the
// owner or the requester close an opener of the given kind.
func closesObligation(openerKind string, fromOwner bool, msg *thread.Entry) bool {
	hasLabel := func(labels ...string) bool {
		return slices.ContainsFunc(msg.Labels, func(label string) bool { return slices.Contains(labels, label) })
	}
	if msg.Kind == format.KindDecision {
		return true
	}
	if !fromOwner {
		if openerKind == format.KindReviewRequest {
			return hasLabel("withdrawn", "done", "merged", "closed")
		}
		return hasLabel("withdrawn", "done", "closed")
	}
	switch openerKind {
	case format.KindReviewRequest:
		return msg.Kind == format.KindReviewResponse || hasLabel("done", "merged", "closed")
	case format.KindQuestion:
		return msg.Kind == format.KindAnswer || hasLabel("done", "closed")
	case format.KindTodo:
		return hasLabel("done", "merged", "closed", "wontfix")
	}
	return false
}

// completionRecipe is the owner message that closes an opener of the kind.
func completionRecipe(openerKind string) threadOpenRecipe {
	switch openerKind {
	case format.KindReviewRequest:
		return threadOpenRecipe{Kind: format.KindReviewResponse, Labels: []string{}}
	case format.KindQuestion:
		return threadOpenRecipe{Kind: format.KindAnswer, Labels: []string{}}
	}
	return threadOpenRecipe{Kind: format.KindStatus, Labels: []string{"done"}}
}

// replyDestination derives the route back to the requester by the rules amq
// reply uses: reply_project, then reply_to, then the sender in the owner's
// session.
func replyDestination(ob *obligation) (*threadOpenDestination, error) {
	opener := ob.opener
	if opener.ReplyProject != "" {
		if strings.TrimSpace(opener.ReplyProject) != opener.ReplyProject {
			return nil, fmt.Errorf("invalid reply_project %q", opener.ReplyProject)
		}
		handle, session, err := parseReplyToRoute(opener.ReplyTo, true)
		if err != nil {
			return nil, err
		}
		return &threadOpenDestination{Handle: handle, Session: session, Project: opener.ReplyProject, Source: "reply_project"}, nil
	}
	if opener.ReplyTo != "" {
		handle, session, err := parseReplyToRoute(opener.ReplyTo, false)
		if err != nil {
			return nil, err
		}
		return &threadOpenDestination{Handle: handle, Session: session, Source: "reply_to"}, nil
	}
	if handle, err := normalizeHandle(opener.From); err != nil || handle != opener.From {
		return nil, fmt.Errorf("invalid sender handle %q", opener.From)
	}
	return &threadOpenDestination{Handle: opener.From, Session: ob.owner.Session, Source: "local"}, nil
}

func obligationJSON(ob *obligation, now time.Time) threadOpenObligation {
	opener := ob.opener
	owedByKind := "agent"
	if ob.owner.Handle == reservedHumanHandle {
		owedByKind = "human"
	}
	out := threadOpenObligation{
		Key:    ob.key,
		Thread: opener.Thread,
		Opener: threadOpenOpener{
			ID:           opener.ID,
			Kind:         opener.Kind,
			From:         opener.From,
			To:           nonNil(opener.To),
			ReplyTo:      opener.ReplyTo,
			ReplyProject: opener.ReplyProject,
			FromProject:  opener.FromProject,
			Subject:      opener.Subject,
			Created:      opener.Created,
			Priority:     opener.Priority,
			Labels:       nonNil(opener.Labels),
		},
		OwedBy:      ob.owner,
		OwedByKind:  owedByKind,
		OwedTo:      ob.requester,
		State:       ob.state,
		Completion:  completionRecipe(opener.Kind),
		Acknowledge: threadOpenRecipe{Kind: format.KindStatus, Labels: []string{}},
		AgeSeconds:  int64(now.Sub(opener.RawTime) / time.Second),
	}
	if dest, err := replyDestination(ob); err != nil {
		out.ReplyDestinationError = err.Error()
	} else {
		out.ReplyDestination = dest
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
		if err := writeStdout("%s  %s owes %s  %s  %s  %s\n", age, participantLabel(ob.OwedBy), participantLabel(ob.OwedTo), ob.Opener.Kind, ob.State, subject); err != nil {
			return err
		}
	}
	cov := result.Coverage
	for _, exp := range cov.Expired {
		if err := writeStdout("expired: %s owes %d older than the cutoff\n", participantLabel(participant{Session: exp.Session, Handle: exp.OwedBy}), exp.Count); err != nil {
			return err
		}
	}
	for _, pruned := range cov.PrunedFiles {
		if err := writeStdout("pruned: %d files in %s older than the cutoff were not read\n", pruned.Count, sessionLabel(pruned.Session)); err != nil {
			return err
		}
	}
	for _, routed := range cov.OutboundRouted {
		if err := writeStdout("routed: %d requests from %s went to a root outside the scan\n", routed.Count, sessionLabel(routed.Session)); err != nil {
			return err
		}
	}
	for _, un := range cov.UnresolvedLineage {
		if err := writeStdout("unresolved: %s from %s refers to missing %s\n", un.ID, un.From, un.MissingRef); err != nil {
			return err
		}
	}
	for _, amb := range cov.Ambiguous {
		if err := writeStdout("ambiguous: %s from %s (%s)\n", amb.ID, amb.From, amb.Reason); err != nil {
			return err
		}
	}
	if cov.TruncatedByLimit.Omitted > 0 {
		if err := writeStdout("limit: %d more not shown\n", cov.TruncatedByLimit.Omitted); err != nil {
			return err
		}
	}
	for _, skip := range cov.Skipped {
		if err := writeStderr("warning: skipping unreadable message %s: %s\n", skip.Path, skip.Error); err != nil {
			return err
		}
	}
	return nil
}

func participantLabel(p participant) string {
	label := p.Handle
	if p.Session != "" {
		label += "@" + p.Session
	}
	if p.Project != "" {
		label += " (" + p.Project + ")"
	}
	return label
}

func sessionLabel(session string) string {
	if session == "" {
		return "the base root"
	}
	return session
}

func countsBySession(sessions []string, counts map[string]int) []threadOpenCount {
	out := []threadOpenCount{}
	for _, session := range sessions {
		if counts[session] > 0 {
			out = append(out, threadOpenCount{Session: session, Count: counts[session]})
		}
	}
	return out
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
