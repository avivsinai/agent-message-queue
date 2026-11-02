package board

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A request is a recorded message of a request kind. The board keeps three
// things about it apart: the message itself (a fact), what the board
// observed about later messages (derived for each view, never stored), and
// opinions on whether it is finished (labels, each attributed to the
// classifier that gave it). No observation or opinion is a lifecycle state.

// RequestWindow is how long a request is Recent, from its recorded creation
// time. Age only partitions the view; it never completes a request.
const RequestWindow = 14 * 24 * time.Hour

// Request age partitions.
const (
	RequestRecent  = "recent"
	RequestOlder   = "older"
	RequestUndated = "undated"
)

// Classification bounds: whole messages only, never clipped.
const (
	requestContextMessages = 8
	requestContextBytes    = 16 << 10
)

// Request label values. Every value except unclear needs a quote.
var requestLabelValues = map[string]string{
	"done":       "likely done",
	"open":       "likely open",
	"superseded": "likely superseded",
	"not_an_ask": "likely not a request",
	"unclear":    "unclear",
}

func isRequestKind(kind string) bool {
	return kind == "todo" || kind == "question" || kind == "review_request"
}

// Participant is a scoped identity. An empty Project is this project; an
// empty Session is the base root.
type Participant struct {
	Project string `json:"project,omitempty"`
	Session string `json:"session"`
	Handle  string `json:"handle"`
}

// RequestRecipient is one addressee and what was observed from it. Session
// is meaningful only when Scoped. LaterAssessed is false when the thread,
// the request time, or the recipient scope is missing: later messages were
// not looked for, so their absence says nothing.
type RequestRecipient struct {
	Handle        string   `json:"handle"`
	Session       string   `json:"session,omitempty"`
	Scoped        bool     `json:"scoped"`
	LaterAssessed bool     `json:"later_assessed"`
	DirectReplies []string `json:"direct_replies"`
	LaterMessages []string `json:"later_messages"`
}

func (r RequestRecipient) participant() Participant {
	return Participant{Session: r.Session, Handle: r.Handle}
}

// unreplied reports a scoped recipient with no direct reply observed.
func (r RequestRecipient) unreplied() bool { return r.Scoped && len(r.DirectReplies) == 0 }

// RequestTask is a task the owner bound to the request: an owner attachment
// or an adopted suggestion. Its state is the tracker's, not the request's.
type RequestTask struct {
	TaskID    string `json:"task_id"`
	Title     string `json:"title"`
	State     State  `json:"recorded_state,omitempty"`
	Cancelled bool   `json:"owner_cancelled,omitempty"`
	Binding   string `json:"binding"`
}

// RequestQuote is exact text from one supplied message's body or subject.
type RequestQuote struct {
	MessageKey string `json:"message_key"`
	Field      string `json:"field"`
	Text       string `json:"text"`
}

// RequestLabel is one classifier's opinion about one request at one
// evidence digest. It never changes facts, observations, or counts.
type RequestLabel struct {
	MessageKey     string        `json:"message_key"`
	EvidenceDigest string        `json:"evidence_digest"`
	Classifier     string        `json:"classifier"`
	Value          string        `json:"value"`
	Quote          *RequestQuote `json:"quote,omitempty"`
}

// RequestLabelKey is the snapshot key of a label: the SHA-256 of the JSON
// array [message key, evidence digest, classifier].
func RequestLabelKey(messageKey, digest, classifier string) string {
	b, _ := json.Marshal([3]string{messageKey, digest, classifier})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Request is one request message with its observations and the opinions
// recorded for its current evidence.
type Request struct {
	Message    Message            `json:"message"`
	Summary    string             `json:"summary,omitempty"`
	Age        string             `json:"age"`
	Sender     *Participant       `json:"sender,omitempty"`
	Recipients []RequestRecipient `json:"recipients"`
	// NoDirectReply is set when some scoped recipient sent no direct reply.
	// It does not mean the request is unfinished.
	NoDirectReply  bool             `json:"no_direct_reply"`
	Tasks          []RequestTask    `json:"tasks,omitempty"`
	EvidenceDigest string           `json:"evidence_digest"`
	Opinions       []RequestOpinion `json:"opinions,omitempty"`
}

// RequestOpinion is a current label with the sender and time of the
// message its quote comes from.
type RequestOpinion struct {
	RequestLabel
	QuoteFrom    string `json:"quote_from,omitempty"`
	QuoteCreated string `json:"quote_created,omitempty"`
}

// RequestCoverage is the AMQ history coverage the observations rest on.
// Complete and Omitted are structured facts; Detail is their wording and
// is never part of an evidence digest.
type RequestCoverage struct {
	Complete bool   `json:"complete"`
	Omitted  int    `json:"omitted"`
	Detail   string `json:"detail,omitempty"`
}

// RequestView is every request outside owner dismissal and cancellation,
// partitioned by age. NoDirectReply is the Recent headline; a request
// whose recipient scope is unknown is counted in UnknownScope instead.
type RequestView struct {
	Recent        []Request       `json:"recent"`
	Older         []Request       `json:"older"`
	Undated       []Request       `json:"undated"`
	NoDirectReply int             `json:"recent_no_direct_reply"`
	UnknownScope  int             `json:"recent_unknown_scope"`
	Coverage      RequestCoverage `json:"coverage"`
}

// requestIndex holds the lookups every request projection needs.
type requestIndex struct {
	s        Snapshot
	project  string
	coverage RequestCoverage
	byID     map[string][]string // namespace#id -> keys
	byThread map[string][]string // namespace#thread -> keys
	lastRef  map[string][]string // namespace#id -> keys whose last ref is id
	referrer map[string][]string // namespace#id -> keys that ref id
	tasks    map[string][]RequestTask
	labels   map[string][]RequestLabel // message key -> labels
}

func newRequestIndex(s Snapshot) requestIndex {
	x := requestIndex{s: s, byID: map[string][]string{}, byThread: map[string][]string{}, lastRef: map[string][]string{}, referrer: map[string][]string{}, tasks: map[string][]RequestTask{}, labels: map[string][]RequestLabel{}}
	for _, nr := range s.Input.Results {
		if nr.Name == "amq" && nr.Result.AMQ != nil {
			x.project = nr.Result.AMQ.Project
		}
		if nr.Name == "amq-history" {
			c := nr.Result.Coverage
			x.coverage = RequestCoverage{Complete: nr.Result.Status == StatusOK && !c.Partial(), Omitted: c.Truncated + c.Skipped + c.Unresolved, Detail: c.Detail}
			if nr.Result.Status != StatusOK && x.coverage.Detail == "" {
				x.coverage.Detail = "AMQ history " + string(nr.Result.Status)
			}
		}
	}
	keys := make([]string, 0, len(s.Messages))
	for key := range s.Messages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m := s.Messages[key]
		x.byID[m.Namespace+"#"+m.ID] = append(x.byID[m.Namespace+"#"+m.ID], key)
		if m.Thread != "" {
			x.byThread[m.Namespace+"#"+m.Thread] = append(x.byThread[m.Namespace+"#"+m.Thread], key)
		}
		seen := map[string]bool{}
		for _, ref := range m.Refs {
			if ref != "" && !seen[ref] {
				seen[ref] = true
				x.referrer[m.Namespace+"#"+ref] = append(x.referrer[m.Namespace+"#"+ref], key)
			}
		}
		if n := len(m.Refs); n > 0 && m.Refs[n-1] != "" {
			x.lastRef[m.Namespace+"#"+m.Refs[n-1]] = append(x.lastRef[m.Namespace+"#"+m.Refs[n-1]], key)
		}
	}
	ids := make([]string, 0, len(s.Tasks))
	for id := range s.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		task := s.Tasks[id]
		if task.RedirectTo != "" {
			continue
		}
		bind := func(key, binding string) {
			for _, t := range x.tasks[key] {
				if t.TaskID == id && t.Binding == binding {
					return
				}
			}
			x.tasks[key] = append(x.tasks[key], RequestTask{TaskID: id, Title: task.Title, State: task.Fact.State, Cancelled: task.Cancelled, Binding: binding})
		}
		for _, key := range task.Attachments {
			bind(key, "owner attachment")
		}
		for _, b := range task.ModelAttachments {
			bind(b.Unit.MessageKey, "owner-adopted suggestion")
		}
	}
	for _, label := range s.RequestLabels {
		x.labels[label.MessageKey] = append(x.labels[label.MessageKey], label)
	}
	return x
}

// rootOf scopes handle to the one copy root of m whose recorded agents name
// it, skipping the session in skip when set. No match or several matches is
// unknown: a copy location alone never proves where a party lives.
func rootOf(m Message, handle string, skip *string) (string, bool) {
	session, found := "", 0
	seen := map[string]bool{}
	for _, c := range m.Copies {
		if seen[c.Session] || skip != nil && c.Session == *skip || !slices.Contains(c.Agents, handle) {
			continue
		}
		seen[c.Session] = true
		session, found = c.Session, found+1
	}
	return session, found == 1
}

// senderOf is the sender's scope. AMQ stamps reply_to (handle@session) on a
// send from a session to another root and reply_project on a cross-project
// send; otherwise the sender is the agent of the one copy root that names it.
func (x requestIndex) senderOf(m Message) (Participant, bool) {
	if m.From == "" {
		return Participant{}, false
	}
	_, session, routed := strings.Cut(m.ReplyTo, "@")
	if m.ReplyProject != "" {
		if x.project == "" {
			return Participant{}, false
		}
		if m.ReplyProject != x.project {
			return Participant{Project: m.ReplyProject, Session: session, Handle: m.From}, true
		}
	}
	if routed {
		return Participant{Session: session, Handle: m.From}, true
	}
	if session, ok := rootOf(m, m.From, nil); ok {
		return Participant{Session: session, Handle: m.From}, true
	}
	return Participant{}, false
}

// recipientOf scopes one addressee of m to the copy root that names it,
// outside the sender's routed session. An addressee in another project is
// unknown.
func (x requestIndex) recipientOf(m Message, handle string) (string, bool) {
	if m.ReplyProject != "" && (x.project == "" || m.ReplyProject == x.project) {
		return "", false
	}
	var skip *string
	if _, session, routed := strings.Cut(m.ReplyTo, "@"); routed && m.ReplyProject == "" {
		skip = &session
	}
	return rootOf(m, handle, skip)
}

func messageTime(m Message) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, m.Created)
	return t, err == nil
}

// request derives one request's observations, evidence digest and current
// opinions.
func (x requestIndex) request(key string, now time.Time) Request {
	m := x.s.Messages[key]
	r := Request{Message: m, Summary: Sentence(m.Body), Age: RequestUndated, Recipients: []RequestRecipient{}, Tasks: x.tasks[key]}
	created, dated := messageTime(m)
	if dated && !created.After(now) {
		r.Age = RequestOlder
		if !created.Before(now.Add(-RequestWindow)) {
			r.Age = RequestRecent
		}
	}
	if sender, ok := x.senderOf(m); ok {
		r.Sender = &sender
	}
	seen := map[string]bool{}
	for _, handle := range m.To {
		if handle == "" || seen[handle] {
			continue
		}
		seen[handle] = true
		session, scoped := x.recipientOf(m, handle)
		rec := RequestRecipient{Handle: handle, Scoped: scoped, DirectReplies: []string{}, LaterMessages: []string{}}
		if scoped {
			rec.Session = session
			for _, reply := range x.lastRef[m.Namespace+"#"+m.ID] {
				if p, ok := x.senderOf(x.s.Messages[reply]); ok && p == rec.participant() {
					rec.DirectReplies = append(rec.DirectReplies, reply)
				}
			}
		}
		rec.LaterAssessed = scoped && dated && m.Thread != ""
		if rec.LaterAssessed {
			for _, other := range x.byThread[m.Namespace+"#"+m.Thread] {
				o := x.s.Messages[other]
				at, ok := messageTime(o)
				if other == key || !ok || !at.After(created) {
					continue
				}
				if p, ok := x.senderOf(o); ok && p == rec.participant() {
					rec.LaterMessages = append(rec.LaterMessages, other)
				}
			}
		}
		r.NoDirectReply = r.NoDirectReply || rec.unreplied()
		r.Recipients = append(r.Recipients, rec)
	}
	r.EvidenceDigest = x.digest(r)
	for _, label := range x.labels[key] {
		if label.EvidenceDigest != r.EvidenceDigest {
			continue
		}
		o := RequestOpinion{RequestLabel: label}
		if label.Quote != nil {
			if q, ok := x.s.Messages[label.Quote.MessageKey]; ok {
				o.QuoteFrom, o.QuoteCreated = q.From, q.Created
			}
		}
		r.Opinions = append(r.Opinions, o)
	}
	sort.Slice(r.Opinions, func(i, j int) bool { return r.Opinions[i].Classifier < r.Opinions[j].Classifier })
	return r
}

// evidence lists the messages an opinion about key depends on, by role:
// the request, its direct parent, direct replies, later messages of its
// thread, and other messages that refer to it. A message whose order
// against the request is unknown counts as later.
func (x requestIndex) evidence(key string) [][2]string {
	m := x.s.Messages[key]
	created, dated := messageTime(m)
	out := [][2]string{{key, "request"}}
	seen := map[string]bool{key: true}
	add := func(role string, keys []string) {
		sorted := append([]string(nil), keys...)
		sort.SliceStable(sorted, func(i, j int) bool { return x.s.Messages[sorted[i]].Created < x.s.Messages[sorted[j]].Created })
		for _, k := range sorted {
			if !seen[k] {
				seen[k] = true
				out = append(out, [2]string{k, role})
			}
		}
	}
	if n := len(m.Refs); n > 0 {
		add("parent", x.byID[m.Namespace+"#"+m.Refs[n-1]])
	}
	add("direct_reply", x.lastRef[m.Namespace+"#"+m.ID])
	if m.Thread != "" {
		var later []string
		for _, other := range x.byThread[m.Namespace+"#"+m.Thread] {
			if at, ok := messageTime(x.s.Messages[other]); dated && ok && at.Before(created) {
				continue
			}
			later = append(later, other)
		}
		add("later", later)
	}
	add("referrer", x.referrer[m.Namespace+"#"+m.ID])
	return out
}

// digest covers every input of the request's observations and of an
// opinion about it: each evidence message with its resolved sender scope,
// including messages omitted from a classification slice, the recipient
// scopes, bound task states, and structured coverage. It excludes the
// clock, free text outside the messages, and the opinions themselves.
func (x requestIndex) digest(r Request) string {
	type evidence struct {
		Key, Hash string
		Sender    Participant
		Scoped    bool
	}
	var messages []evidence
	for _, e := range x.evidence(r.Message.Key) {
		m := x.s.Messages[e[0]]
		sender, ok := x.senderOf(m)
		messages = append(messages, evidence{e[0], m.EvidenceHash, sender, ok})
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].Key < messages[j].Key })
	type scope struct {
		Handle, Session string
		Scoped          bool
	}
	var recipients []scope
	for _, rec := range r.Recipients {
		recipients = append(recipients, scope{rec.Handle, rec.Session, rec.Scoped})
	}
	type task struct {
		ID, Binding string
		State       State
		Cancelled   bool
	}
	var tasks []task
	for _, t := range r.Tasks {
		tasks = append(tasks, task{t.TaskID, t.Binding, t.State, t.Cancelled})
	}
	return digestValue(struct {
		Schema     string
		Messages   []evidence
		Recipients []scope
		Tasks      []task
		Dismissed  bool
		Cancelled  bool
		Complete   bool
		Omitted    int
	}{"amq/board-request-evidence/v1", messages, recipients, tasks, x.s.Dismissed[r.Message.Key], x.s.ActionCancelled[r.Message.Key], x.coverage.Complete, x.coverage.Omitted})
}

// RequestView projects every request the owner has not dismissed or
// cancelled. Recent requests are grouped by the renderers; here they are
// ordered no-direct-reply first, then newest first.
func (s Snapshot) RequestView(now time.Time) RequestView {
	x := newRequestIndex(s)
	v := RequestView{Recent: []Request{}, Older: []Request{}, Undated: []Request{}, Coverage: x.coverage}
	for _, key := range x.candidates() {
		r := x.request(key, now)
		switch r.Age {
		case RequestRecent:
			v.Recent = append(v.Recent, r)
			scoped := false
			for _, rec := range r.Recipients {
				scoped = scoped || rec.Scoped
			}
			switch {
			case !scoped:
				v.UnknownScope++
			case r.NoDirectReply:
				v.NoDirectReply++
			}
		case RequestOlder:
			v.Older = append(v.Older, r)
		default:
			v.Undated = append(v.Undated, r)
		}
	}
	sort.SliceStable(v.Recent, func(i, j int) bool {
		if a, b := v.Recent[i].NoDirectReply, v.Recent[j].NoDirectReply; a != b {
			return a
		}
		return askNewer(v.Recent[i].Message, v.Recent[j].Message)
	})
	for _, list := range [][]Request{v.Older, v.Undated} {
		sort.SliceStable(list, func(i, j int) bool { return askNewer(list[i].Message, list[j].Message) })
	}
	return v
}

// candidates are the request messages outside owner dismissal and action
// cancellation, in key order. Attachment to a task does not remove one.
func (x requestIndex) candidates() []string {
	var keys []string
	for key, m := range x.s.Messages {
		if isRequestKind(m.Kind) && !x.s.Dismissed[key] && !x.s.ActionCancelled[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// RequestGroup is the Recent requests of one addressee.
type RequestGroup struct {
	Addressee string
	Requests  []Request
}

// recentGroups groups Recent requests by scoped addressee: groups with more
// requests that have no direct reply from that addressee first, then by
// name. A request with several recipients appears under each; one whose
// recipient scope is unknown is grouped under its bare handles.
func recentGroups(v RequestView) []RequestGroup {
	index := map[string]int{}
	var out []RequestGroup
	for _, r := range v.Recent {
		names := map[string]bool{}
		for _, rec := range r.Recipients {
			name := rec.Handle + " · scope unknown"
			if rec.Scoped {
				name = participantLabel(rec.participant())
			}
			if names[name] {
				continue
			}
			names[name] = true
			i, ok := index[name]
			if !ok {
				i = len(out)
				index[name] = i
				out = append(out, RequestGroup{Addressee: name})
			}
			out[i].Requests = append(out[i].Requests, r)
		}
	}
	for i := range out {
		g := out[i]
		unreplied := func(r Request) bool {
			for _, rec := range r.Recipients {
				name := rec.Handle + " · scope unknown"
				if rec.Scoped {
					name = participantLabel(rec.participant())
				}
				if name == g.Addressee {
					return rec.unreplied()
				}
			}
			return false
		}
		sort.SliceStable(g.Requests, func(a, b int) bool {
			if sa, sb := unreplied(g.Requests[a]), unreplied(g.Requests[b]); sa != sb {
				return sa
			}
			return askNewer(g.Requests[a].Message, g.Requests[b].Message)
		})
	}
	unrepliedCount := func(g RequestGroup) int {
		n := 0
		for _, r := range g.Requests {
			for _, rec := range r.Recipients {
				if rec.unreplied() && rec.Scoped && participantLabel(rec.participant()) == g.Addressee {
					n++
				}
			}
		}
		return n
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := unrepliedCount(out[i]), unrepliedCount(out[j]); a != b {
			return a > b
		}
		return out[i].Addressee < out[j].Addressee
	})
	return out
}

func participantLabel(p Participant) string {
	s := p.Handle + " in "
	if p.Session == "" {
		s += "base root"
	} else {
		s += p.Session
	}
	if p.Project != "" {
		s += " of " + p.Project
	}
	return s
}

// requestHeadline is the only request count the board states, with its
// qualifications. It is never a count of unfinished work.
func requestHeadline(v RequestView) (string, []string) {
	head := pluralize(v.NoDirectReply, "recent request") + " with no direct reply observed"
	var notes []string
	if !v.Coverage.Complete {
		notes = append(notes, "History coverage incomplete; see Sources")
	}
	if v.UnknownScope > 0 {
		notes = append(notes, pluralize(v.UnknownScope, "recent request")+" with unknown recipient scope not assessed")
	}
	return head, notes
}

// requestFact is one recipient's observations in words. State is unreplied
// (no direct reply), replied, or unknown (scope unknown, not assessed).
type requestFact struct{ State, Text string }

// requestFacts are a request's observations, one per recipient: the direct
// reply first, then later activity in the thread as a secondary fact.
func requestFacts(r Request) []requestFact {
	var out []requestFact
	for _, rec := range r.Recipients {
		if !rec.Scoped {
			out = append(out, requestFact{"unknown", rec.Handle + ": recipient scope unknown; not assessed"})
			continue
		}
		f := requestFact{State: "unreplied", Text: "no direct reply observed"}
		switch n := len(rec.DirectReplies); {
		case n == 1:
			f.State, f.Text = "replied", "1 direct reply"
		case n > 1:
			f.State, f.Text = "replied", strconv.Itoa(n)+" direct replies"
		}
		switch {
		case !rec.LaterAssessed && r.Message.Thread == "":
			f.Text += "; no thread"
		case !rec.LaterAssessed:
			f.Text += "; request time unknown"
		case len(rec.LaterMessages) == 0:
			f.Text += "; no later message in this thread"
		default:
			f.Text += "; " + pluralize(len(rec.LaterMessages), "later message") + " in this thread"
		}
		f.Text = participantLabel(rec.participant()) + ": " + f.Text
		out = append(out, f)
	}
	if len(out) == 0 {
		out = append(out, requestFact{"unknown", "no recipient recorded"})
	}
	return out
}

// quoteSource names the sender and age of an opinion's quoted message,
// never its key.
func quoteSource(o RequestOpinion, now time.Time) string {
	if o.QuoteFrom == "" {
		return ""
	}
	src := "from " + o.QuoteFrom
	if at, err := time.Parse(time.RFC3339Nano, o.QuoteCreated); err == nil {
		src += ", " + age(now.Sub(at)) + " ago"
	}
	return src
}

// opinionText is one attributed opinion in words.
func opinionText(l RequestOpinion) string {
	return requestLabelValues[l.Value] + " · " + l.Classifier
}
