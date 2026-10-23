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

// threadOpenUnsettledParent is the ambiguity reason for a same-kind reply
// whose parent's lineage is not settled at its created time.
const threadOpenUnsettledParent = "parent lineage not settled at created time"

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
	SkewDeferred      []threadOpenCount      `json:"skew_deferred"`
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
	Reason     string `json:"reason"` // missing, skipped, or cycle
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
	result.Coverage.SkewDeferred = countsBySession(result.Sessions, red.deferred)

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

// record is one message with its resolved sender, its direct parent, and
// the obligations whose lineage it belongs to.
type record struct {
	entry      *thread.Entry
	sender     participant
	hops       int     // records read by name on the way to this one
	parent     *record // direct target (last ref); nil without refs or when missing
	opens      []string
	lineage    []*obligation
	ready      bool                  // lineage is settled
	waiters    []*record             // messages whose acting waits for this lineage
	unresolved *threadOpenUnresolved // set when the lineage reaches no root
	classified bool
}

// obligationReducer applies the thread open rules in one pass in created
// order, then id.
//
// Every header is classified from headers alone before the pass: its direct
// parent and whether its lineage reaches a root. Openers open at
// their own created time, so no opener waits for its parent. A message
// without refs that opens nothing is matched against the obligations open
// at that moment (rule 5). A message with refs acts on its parent's lineage
// as soon as that lineage is settled; when the parent's lineage settles
// later (clock skew), the action is deferred until then and counted in
// skew_deferred.
type obligationReducer struct {
	roots       []thread.ScanRoot
	cutoff      time.Time
	records     map[string]*record
	order       []*record
	obligations []*obligation
	byThread    map[string][]*obligation
	lookups     int
	skipped     []threadOpenSkipped
	unresolved  []threadOpenUnresolved
	ambiguous   []threadOpenAmbiguous
	outbound    map[string]int
	deferred    map[string]int
}

func newObligationReducer(roots []thread.ScanRoot, entries []thread.Entry, cutoff time.Time) *obligationReducer {
	red := &obligationReducer{
		roots:      roots,
		cutoff:     cutoff,
		records:    map[string]*record{},
		byThread:   map[string][]*obligation{},
		skipped:    []threadOpenSkipped{},
		unresolved: []threadOpenUnresolved{},
		ambiguous:  []threadOpenAmbiguous{},
		outbound:   map[string]int{},
		deferred:   map[string]int{},
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

func (red *obligationReducer) run() {
	// Load: read each missing direct parent by name, so the message set is
	// fixed before anything is classified.
	for i := 0; i < len(red.order); i++ {
		if refs := red.order[i].entry.Refs; len(refs) > 0 {
			if found := red.lookup(refs[len(refs)-1], red.order[i]); found != nil {
				red.order = append(red.order, found)
			}
		}
	}
	sort.SliceStable(red.order, func(i, j int) bool {
		a, b := red.order[i].entry, red.order[j].entry
		if !a.RawTime.Equal(b.RawTime) {
			return a.RawTime.Before(b.RawTime)
		}
		return a.ID < b.ID
	})

	// Classify from headers alone: the direct parent, whether the lineage
	// reaches a root, and the recipients each message opens for.
	for _, rec := range red.order {
		if refs := rec.entry.Refs; len(refs) > 0 {
			rec.parent = red.records[refs[len(refs)-1]]
		}
	}
	for _, rec := range red.order {
		red.classifyLineage(rec, map[*record]bool{})
	}

	for _, rec := range red.order {
		msg := rec.entry
		if msg.RawTime.IsZero() {
			red.skipped = append(red.skipped, threadOpenSkipped{Path: msg.Occurrences[0].Path, Error: fmt.Sprintf("invalid created timestamp %q", msg.Created)})
			continue
		}
		if rec.unresolved != nil {
			// Unresolved lineage is reported and never becomes work.
			red.unresolved = append(red.unresolved, *rec.unresolved)
			continue
		}
		rec.opens = red.opensFor(rec)
		if len(msg.Refs) == 0 {
			if len(rec.opens) > 0 {
				red.settle(rec, red.open(rec))
			} else if !slices.Contains(openerKinds, msg.Kind) || isTerminal(msg) {
				red.settle(rec, red.actWithoutRefs(rec))
			} else {
				red.settle(rec, nil)
			}
			continue
		}
		if opened := red.open(rec); len(opened) > 0 {
			red.settle(rec, opened)
		}
		if rec.parent.ready {
			red.actOnParent(rec)
		} else {
			rec.parent.waiters = append(rec.parent.waiters, rec)
			red.deferred[rec.sender.Session]++
		}
	}
}

// classifyLineage marks rec unresolved when following direct parents does
// not reach a root: a parent that is missing, a parent whose created does
// not parse, or a reference cycle. A message below an unresolved parent
// inherits the root's missing ref and reason.
func (red *obligationReducer) classifyLineage(rec *record, visiting map[*record]bool) *threadOpenUnresolved {
	if rec.classified || len(rec.entry.Refs) == 0 {
		rec.classified = true
		return rec.unresolved
	}
	parentID := rec.entry.Refs[len(rec.entry.Refs)-1]
	reason := ""
	missing := parentID
	switch {
	case rec.parent == nil:
		reason = "missing"
	case rec.parent.entry.RawTime.IsZero():
		reason = "skipped"
	case visiting[rec.parent]:
		reason = "cycle"
	default:
		visiting[rec] = true
		if up := red.classifyLineage(rec.parent, visiting); up != nil {
			reason, missing = up.Reason, up.MissingRef
		}
		delete(visiting, rec)
	}
	rec.classified = true
	if reason != "" {
		rec.unresolved = &threadOpenUnresolved{
			ID: rec.entry.ID, Session: rec.sender.Session, From: rec.entry.From, Kind: rec.entry.Kind, MissingRef: missing, Reason: reason,
		}
	}
	return rec.unresolved
}

// actOnParent applies a message with refs to its parent's settled lineage:
// close, then acknowledge, for the owner or the requester addressed to the
// other party; a message from the owner to someone else acknowledges.
func (red *obligationReducer) actOnParent(rec *record) {
	msg := rec.entry
	var acted []*obligation
	for _, ob := range rec.parent.lineage {
		owner, requester := roleOf(ob, rec.sender, true)
		if !owner && !requester {
			continue
		}
		if !slices.Contains(msg.To, otherParty(ob, owner).Handle) {
			if owner {
				acknowledge(ob, msg)
			}
			continue
		}
		act(ob, owner, msg)
		acted = append(acted, ob)
	}
	if !rec.ready {
		red.settle(rec, acted)
	}
}

// settle fixes the lineage of rec and runs the actions that waited for it.
func (red *obligationReducer) settle(rec *record, lineage []*obligation) {
	rec.lineage = lineage
	rec.ready = true
	waiters := rec.waiters
	rec.waiters = nil
	for _, waiter := range waiters {
		red.actOnParent(waiter)
	}
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

// opensFor lists the recipients a message opens an obligation for. Only an
// opener kind without a closing label opens. A message in the kind reply
// would default to continues an obligation in its parent's lineage for a
// recipient when the sender and that recipient are that obligation's two
// parties; any other recipient gets an obligation of their own. When the
// parent's lineage is not settled at the message's created time (the
// parent, or an ancestor it waits for, is created later), that cannot be
// decided: the message opens nothing and is reported ambiguous.
func (red *obligationReducer) opensFor(rec *record) []string {
	msg := rec.entry
	if !slices.Contains(openerKinds, msg.Kind) || isTerminal(msg) {
		return nil
	}
	p := rec.parent
	continuation := p != nil && msg.Kind == defaultReplyKind(p.entry.Kind)
	if continuation && !p.ready {
		red.ambiguous = append(red.ambiguous, threadOpenAmbiguous{
			ID: msg.ID, Session: rec.sender.Session, From: msg.From, Reason: threadOpenUnsettledParent, Candidates: []string{},
		})
		return nil
	}
	opens := []string{}
	for _, handle := range slices.Compact(slices.Sorted(slices.Values(msg.To))) {
		if continuation {
			if r, ok := recipientOf(rec, handle); ok && continuesPair(rec, r) {
				continue
			}
		}
		opens = append(opens, handle)
	}
	return opens
}

// continuesPair reports whether rec, sent to recipient r, continues an
// obligation of its parent's settled lineage between the same two parties.
func continuesPair(rec *record, r participant) bool {
	for _, ob := range rec.parent.lineage {
		owner, requester := roleOf(ob, rec.sender, true)
		if (owner || requester) && otherParty(ob, owner) == r {
			return true
		}
	}
	return false
}

// actWithoutRefs links a message without refs to the one obligation open at
// its created time in its thread between the same two exact participants.
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

// open creates one obligation per resolved recipient in rec.opens.
func (red *obligationReducer) open(rec *record) []*obligation {
	msg := rec.entry
	opened := []*obligation{}
	routed := false
	for _, handle := range rec.opens {
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
