package thread

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Obligation states while an obligation is open.
const (
	StatePending      = "pending"
	StateAcknowledged = "acknowledged"
)

const (
	// A continuation whose direct target is not in the entries is looked up
	// by file name: at most this many missing hops per message and lookups
	// per reduction.
	maxHops    = 8
	maxLookups = 512
)

// UnsettledParent is the ambiguity reason for a same-kind reply whose
// parent's lineage is not settled at its created time.
const UnsettledParent = "parent lineage not settled at created time"

// OpenerKinds are the kinds that open an obligation for each recipient.
var OpenerKinds = []string{format.KindReviewRequest, format.KindQuestion, format.KindTodo}

// closingLabels make a message terminal: it never opens an obligation.
var closingLabels = []string{"done", "merged", "closed", "wontfix", "withdrawn"}

// Participant is one party to an obligation. Project is empty for the
// project under scan; Session is empty for a base root. Handles alone never
// identify a participant.
type Participant struct {
	Project string `json:"project"`
	Session string `json:"session"`
	Handle  string `json:"handle"`
}

// Obligation is the debt of one recipient (Owner) to the sender
// (Requester) of one opener message.
type Obligation struct {
	Key       string
	Opener    *Entry
	Owner     Participant
	Requester Participant
	State     string
	Closed    bool
	// Expired marks an opener created before the reduction's cutoff.
	Expired bool
	Last    *Entry
}

// Open reports whether the obligation is still owed: neither closed nor
// created before the cutoff.
func (ob *Obligation) Open() bool {
	return !ob.Closed && !ob.Expired
}

// Skipped is a message whose header cannot be reduced.
type Skipped struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Unresolved is a message whose lineage reaches no root.
type Unresolved struct {
	ID         string `json:"id"`
	Session    string `json:"session"`
	From       string `json:"from"`
	Kind       string `json:"kind"`
	MissingRef string `json:"missing_ref"`
	Reason     string `json:"reason"` // missing, skipped, or cycle
}

// Ambiguous is a message whose effect on obligations cannot be decided.
type Ambiguous struct {
	ID         string   `json:"id"`
	Session    string   `json:"session"`
	From       string   `json:"from"`
	Reason     string   `json:"reason"`
	Candidates []string `json:"candidates"`
}

// RoutedOpener is an opener with a recipient outside the scanned roots.
type RoutedOpener struct {
	ID      string `json:"id"`
	Session string `json:"session"`
}

// Reduction is the result of ReduceObligations: every obligation in opener
// created order, then id, and what the reduction could not judge.
type Reduction struct {
	Obligations     []*Obligation
	Skipped         []Skipped
	Unresolved      []Unresolved
	Ambiguous       []Ambiguous
	Outbound        map[string]int // routed openers per sender session
	OutboundOpeners []RoutedOpener
	Deferred        map[string]int // actions deferred for clock skew per sender session

	roots    []ScanRoot
	cutoff   time.Time
	human    string
	records  map[string]*record
	order    []*record
	byThread map[string][]*Obligation
	lookups  int
}

// record is one message with its resolved sender, its direct parent, and
// the obligations whose lineage it belongs to.
type record struct {
	entry      *Entry
	sender     Participant
	hops       int     // records read by name on the way to this one
	parent     *record // direct target (last ref); nil without refs or when missing
	opens      []string
	lineage    []*Obligation
	ready      bool        // lineage is settled
	waiters    []*record   // messages whose acting waits for this lineage
	unresolved *Unresolved // set when the lineage reaches no root
	classified bool
}

// ReduceObligations applies the obligation rules to entries in one pass in
// created order, then id. A direct target missing from entries is read by
// name from roots; with no roots nothing is read. An opener created before a
// nonzero cutoff is Expired. human is the reserved human handle of the
// project under scan.
//
// Every header is classified from headers alone before the pass: its direct
// parent and whether its lineage reaches a root. Openers open at their own
// created time, so no opener waits for its parent. A message without refs
// that opens nothing is matched against the obligations open at that moment.
// A message with refs acts on its parent's lineage as soon as that lineage
// is settled; when the parent's lineage settles later (clock skew), the
// action is deferred until then and counted in Deferred.
func ReduceObligations(roots []ScanRoot, entries []Entry, cutoff time.Time, human string) *Reduction {
	red := &Reduction{
		Skipped:         []Skipped{},
		Unresolved:      []Unresolved{},
		Ambiguous:       []Ambiguous{},
		Outbound:        map[string]int{},
		OutboundOpeners: []RoutedOpener{},
		Deferred:        map[string]int{},
		roots:           roots,
		cutoff:          cutoff,
		human:           human,
		records:         map[string]*record{},
		byThread:        map[string][]*Obligation{},
	}
	for i := range entries {
		red.order = append(red.order, red.add(&entries[i]))
	}
	red.run()
	return red
}

func (red *Reduction) add(entry *Entry) *record {
	rec := &record{entry: entry, sender: senderOf(entry)}
	red.records[entry.ID] = rec
	return rec
}

func (red *Reduction) run() {
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
			red.Skipped = append(red.Skipped, Skipped{Path: msg.Occurrences[0].Path, Error: fmt.Sprintf("invalid created timestamp %q", msg.Created)})
			continue
		}
		if rec.unresolved != nil {
			// Unresolved lineage is reported and never becomes work.
			red.Unresolved = append(red.Unresolved, *rec.unresolved)
			continue
		}
		rec.opens = red.opensFor(rec)
		if len(msg.Refs) == 0 {
			if len(rec.opens) > 0 {
				red.settle(rec, red.open(rec))
			} else if !slices.Contains(OpenerKinds, msg.Kind) || isTerminal(msg) {
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
			red.Deferred[rec.sender.Session]++
		}
	}
}

// classifyLineage marks rec unresolved when following direct parents does
// not reach a root: a parent that is missing, a parent whose created does
// not parse, or a reference cycle. A message below an unresolved parent
// inherits the root's missing ref and reason.
func (red *Reduction) classifyLineage(rec *record, visiting map[*record]bool) *Unresolved {
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
		rec.unresolved = &Unresolved{
			ID: rec.entry.ID, Session: rec.sender.Session, From: rec.entry.From, Kind: rec.entry.Kind, MissingRef: missing, Reason: reason,
		}
	}
	return rec.unresolved
}

// actOnParent applies a message with refs to its parent's settled lineage:
// close, then acknowledge, for the owner or the requester addressed to the
// other party; a message from the owner to someone else acknowledges.
func (red *Reduction) actOnParent(rec *record) {
	msg := rec.entry
	var acted []*Obligation
	for _, ob := range rec.parent.lineage {
		owner, requester := red.roleOf(ob, rec.sender, true)
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
func (red *Reduction) settle(rec *record, lineage []*Obligation) {
	rec.lineage = lineage
	rec.ready = true
	waiters := rec.waiters
	rec.waiters = nil
	for _, waiter := range waiters {
		red.actOnParent(waiter)
	}
}

// lookup reads the header of a direct target the entries do not hold, by
// name, and returns the new record; nil when it is held already or not
// found. A chain of records read by name is at most maxHops long.
func (red *Reduction) lookup(id string, from *record) *record {
	if _, ok := red.records[id]; ok {
		return nil
	}
	if len(red.roots) == 0 || from.hops >= maxHops || red.lookups >= maxLookups {
		return nil
	}
	red.lookups++
	agents := append([]string{from.entry.From}, from.entry.To...)
	entry, ok := ReadByID(red.roots, agents, id)
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
func (red *Reduction) opensFor(rec *record) []string {
	msg := rec.entry
	if !slices.Contains(OpenerKinds, msg.Kind) || isTerminal(msg) {
		return nil
	}
	p := rec.parent
	continuation := p != nil && msg.Kind == defaultReplyKind(p.entry.Kind)
	if continuation && !p.ready {
		red.Ambiguous = append(red.Ambiguous, Ambiguous{
			ID: msg.ID, Session: rec.sender.Session, From: msg.From, Reason: UnsettledParent, Candidates: []string{},
		})
		return nil
	}
	opens := []string{}
	for _, handle := range slices.Compact(slices.Sorted(slices.Values(msg.To))) {
		if continuation {
			if r, ok := recipientOf(rec, handle); ok && red.continuesPair(rec, r) {
				continue
			}
		}
		opens = append(opens, handle)
	}
	return opens
}

// continuesPair reports whether rec, sent to recipient r, continues an
// obligation of its parent's settled lineage between the same two parties.
func (red *Reduction) continuesPair(rec *record, r Participant) bool {
	for _, ob := range rec.parent.lineage {
		owner, requester := red.roleOf(ob, rec.sender, true)
		if (owner || requester) && otherParty(ob, owner) == r {
			return true
		}
	}
	return false
}

// actWithoutRefs links a message without refs to the one obligation open at
// its created time in its thread between the same two exact participants.
func (red *Reduction) actWithoutRefs(rec *record) []*Obligation {
	msg := rec.entry
	recipients := []Participant{}
	unresolved := false
	for _, handle := range msg.To {
		if p, ok := recipientOf(rec, handle); ok {
			recipients = append(recipients, p)
		} else {
			unresolved = true
		}
	}
	candidates := []*Obligation{}
	for _, ob := range red.byThread[msg.Thread] {
		if ob.Closed || !ob.Opener.RawTime.Before(msg.RawTime) {
			continue
		}
		owner, requester := red.roleOf(ob, rec.sender, false)
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
			keys = append(keys, ob.Key)
		}
		red.Ambiguous = append(red.Ambiguous, Ambiguous{
			ID: msg.ID, Session: rec.sender.Session, From: msg.From, Reason: reason, Candidates: keys,
		})
		return nil
	}
	owner, _ := red.roleOf(candidates[0], rec.sender, false)
	act(candidates[0], owner, msg)
	return candidates
}

// open creates one obligation per resolved recipient in rec.opens.
func (red *Reduction) open(rec *record) []*Obligation {
	msg := rec.entry
	opened := []*Obligation{}
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
		ob := &Obligation{
			Key:       session + "/" + msg.ID + "/" + owner.Handle,
			Opener:    msg,
			Owner:     owner,
			Requester: rec.sender,
			State:     StatePending,
			Expired:   !red.cutoff.IsZero() && msg.RawTime.Before(red.cutoff),
		}
		opened = append(opened, ob)
		red.Obligations = append(red.Obligations, ob)
		red.byThread[msg.Thread] = append(red.byThread[msg.Thread], ob)
	}
	if routed {
		red.Outbound[rec.sender.Session]++
		red.OutboundOpeners = append(red.OutboundOpeners, RoutedOpener{ID: msg.ID, Session: rec.sender.Session})
	}
	return opened
}

// roleOf reports whether sender is the owner or the requester of ob. Through
// an exact reference, the reserved human of the project under scan acts as
// the owner from any of its sessions.
func (red *Reduction) roleOf(ob *Obligation, sender Participant, byReference bool) (owner, requester bool) {
	switch {
	case sender == ob.Owner:
		return true, false
	case sender == ob.Requester:
		return false, true
	case byReference && sender.Handle == red.human && sender.Project == "" &&
		ob.Owner.Handle == red.human && ob.Owner.Project == "":
		return true, false
	}
	return false, false
}

// otherParty is the participant a message from the owner (or, when owner is
// false, from the requester) of ob must be addressed to.
func otherParty(ob *Obligation, owner bool) Participant {
	if owner {
		return ob.Requester
	}
	return ob.Owner
}

func act(ob *Obligation, fromOwner bool, msg *Entry) {
	touch(ob, msg)
	if ob.Closed {
		return
	}
	if closesObligation(ob.Opener.Kind, fromOwner, msg) {
		ob.Closed = true
		return
	}
	if fromOwner {
		ob.State = StateAcknowledged
	}
}

func acknowledge(ob *Obligation, msg *Entry) {
	touch(ob, msg)
	if !ob.Closed {
		ob.State = StateAcknowledged
	}
}

func touch(ob *Obligation, msg *Entry) {
	if ob.Last == nil || ob.Last.RawTime.Before(msg.RawTime) {
		ob.Last = msg
	}
}

// senderOf resolves who sent msg: the sender's outbox copy wins; otherwise
// the routing fields; otherwise the session of the first copy, where a local
// message is delivered. Every entry has at least one occurrence.
func senderOf(msg *Entry) Participant {
	for _, occ := range msg.Occurrences {
		if occ.Dir == fsq.MailboxOutboxSent {
			return Participant{Session: occ.Session, Handle: occ.Agent}
		}
	}
	_, replySession, _ := strings.Cut(msg.ReplyTo, "@")
	if msg.ReplyProject != "" {
		return Participant{Project: msg.ReplyProject, Session: replySession, Handle: msg.From}
	}
	if msg.ReplyTo != "" {
		return Participant{Session: replySession, Handle: msg.From}
	}
	return Participant{Session: msg.Occurrences[0].Session, Handle: msg.From}
}

// recipientOf resolves a recipient handle of a message: the session of its
// inbox copy; else the sender's session for a local message. A routed
// message with no inbox copy in scope has no target metadata, so its
// recipient is unresolved.
func recipientOf(rec *record, handle string) (Participant, bool) {
	for _, occ := range rec.entry.Occurrences {
		if occ.Agent == handle && (occ.Dir == fsq.MailboxInboxNew || occ.Dir == fsq.MailboxInboxCur) {
			return Participant{Session: occ.Session, Handle: handle}, true
		}
	}
	if rec.entry.ReplyTo == "" && rec.sender.Project == "" {
		return Participant{Session: rec.sender.Session, Handle: handle}, true
	}
	return Participant{}, false
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

func isTerminal(msg *Entry) bool {
	return slices.ContainsFunc(msg.Labels, func(label string) bool { return slices.Contains(closingLabels, label) })
}

// closesObligation applies the closer table: which kinds and labels from the
// owner or the requester close an opener of the given kind.
func closesObligation(openerKind string, fromOwner bool, msg *Entry) bool {
	hasLabel := func(labels ...string) bool {
		return slices.ContainsFunc(msg.Labels, func(label string) bool { return slices.Contains(labels, label) })
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
