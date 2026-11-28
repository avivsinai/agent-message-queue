package board

import (
	"cmp"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// OwnerItem is one thing that waits on the owner.
type OwnerItem struct {
	ID          string        `json:"id"`   // the card or request it links to
	Kind        string        `json:"kind"` // question, review_request, todo, bead, task
	Bead        string        `json:"bead,omitempty"`
	Title       string        `json:"title"`
	Summary     string        `json:"summary,omitempty"`
	From        string        `json:"from,omitempty"`
	Since       time.Time     `json:"since"`
	ReplyBy     *time.Time    `json:"reply_by,omitempty"`
	Blocking    bool          `json:"blocking"`
	Options     []OwnerOption `json:"options,omitempty"`
	Recommended string        `json:"recommended,omitempty"`
	IfLater     string        `json:"if_later,omitempty"`
	// Answer is one command per option (the option label is the body), then
	// the free-text answer and the acknowledgement.
	Answer []Action `json:"answer,omitempty"`
	// NoAnswer says why a request has no answer command.
	NoAnswer string  `json:"no_answer,omitempty"`
	Read     *Action `json:"read,omitempty"`
	// Link is the URL the owner acts through, set for a pull request that
	// asks for the owner's review.
	Link string `json:"link,omitempty"`
}

// OwnerOption is one option of a decision.
type OwnerOption struct {
	Label   string   `json:"label"`
	Summary string   `json:"summary,omitempty"`
	Pros    []string `json:"pros,omitempty"`
	Cons    []string `json:"cons,omitempty"`
}

// OwnerMail is the owner's unread mail in one session. It is never an item.
type OwnerMail struct {
	Session          string  `json:"session"`
	Unread           int     `json:"unread"`
	OldestAgeSeconds float64 `json:"oldest_age_seconds"`
	Read             *Action `json:"read,omitempty"`
}

// OwnerOldMail is the unread mail older than the obligation age, folded to
// one line so old mail never shouts; Sessions keeps one row per old session
// with its read command, so nothing the open mail rows gave becomes
// unreachable.
type OwnerOldMail struct {
	Sessions          int         `json:"sessions"`
	Unread            int         `json:"unread"`
	OldestAgeSeconds  float64     `json:"oldest_age_seconds"`
	OldestSessionList string      `json:"oldest_sessions"`
	Rows              []OwnerMail `json:"rows,omitempty"`
}

// Inbox is the owner's queue: items, blocking first, then oldest first; then
// the mail lines, split at the obligation age into per-session rows and one
// folded line for older mail.
type Inbox struct {
	Items   []OwnerItem   `json:"items"`
	Mail    []OwnerMail   `json:"mail,omitempty"`
	OldMail *OwnerOldMail `json:"old_mail,omitempty"`
	MaxAge  string        `json:"mail_max_age,omitempty"`
}

// buildInbox fills m.Inbox from the one rule of what waits on the owner: the
// cards in Needs you (including an unclaimed pull request whose open item
// waits on the human) and the open obligations owed by the owner handle. A
// question's options come from its opener's context.decision, parsed by the
// rule send applies; an invalid or absent decision gives a plain item.
func buildInbox(m *Model, messages map[string]Message, sessionPaths map[string]string, peers map[string]bool, cfg Config) {
	inbox := Inbox{Items: []OwnerItem{}}
	seenQuestions := m.SeenQuestions
	if seenQuestions == nil {
		seenQuestions = map[string]bool{}
	}
	for _, col := range m.Columns {
		if col.ID != ColumnWaiting {
			continue
		}
		for _, c := range col.Cards {
			it := cardItem(c, peers, seenQuestions)
			if it.Kind != "hidden" {
				inbox.Items = append(inbox.Items, it)
			}
		}
	}
	for _, c := range m.Forge {
		if cardFlags(c) != nil {
			inbox.Items = append(inbox.Items, forgeItem(c))
		}
	}
	for _, a := range m.Asks {
		if !a.Human {
			continue
		}
		message := messages[a.Key]
		it := OwnerItem{ID: a.Key, Kind: a.Kind, Title: a.Subject, Summary: Sentence(message.Body), From: participantLabel(a.From), Since: a.Opened}
		if it.Title == "" {
			it.Title = a.Kind + " from " + it.From
		}
		if !a.ReplyBy.IsZero() {
			due := a.ReplyBy
			it.ReplyBy = &due
		}
		var labels []string
		if d, ok, err := format.ParseDecision(message.Context); ok && err == nil {
			it.Blocking, it.Recommended, it.IfLater = d.Blocking, d.Recommended, d.IfLater
			for _, o := range d.Options {
				it.Options = append(it.Options, OwnerOption(o))
				labels = append(labels, o.Label)
			}
		}
		it.Answer, it.NoAnswer = answerItem(a.answer, peers, labels)
		if a.answer != nil {
			if read, ok := ShowThread(sessionPaths[a.answer.OwnerSession], a.answer.Thread); ok {
				it.Read = &read
			}
		}
		inbox.Items = append(inbox.Items, it)
	}
	slices.SortStableFunc(inbox.Items, func(a, b OwnerItem) int {
		if a.Blocking != b.Blocking {
			if a.Blocking {
				return -1
			}
			return 1
		}
		return cmp.Or(a.Since.Compare(b.Since), cmp.Compare(a.ID, b.ID))
	})
	inbox.MaxAge = obligationMaxAgeString(cfg)
	for _, g := range m.Gates {
		mail := OwnerMail{Session: g.Session, Unread: g.OpenCount, OldestAgeSeconds: g.OldestAgeSeconds}
		if read, ok := ReadOwnerMail(g.Session, g.OpenCount); ok {
			mail.Read = &read
		}
		inbox.Mail = append(inbox.Mail, mail)
	}
	if len(inbox.Mail) > 0 {
		maxSeconds := obligationMaxAgeSeconds(cfg)
		var old []OwnerMail
		kept := inbox.Mail[:0]
		for _, mail := range inbox.Mail {
			// The same boundary as the ask cutoff: mail whose oldest
			// message is past the obligation age folds (a zero cutoff
			// folds nothing).
			if maxSeconds > 0 && mail.OldestAgeSeconds > maxSeconds {
				old = append(old, mail)
			} else {
				kept = append(kept, mail)
			}
		}
		inbox.Mail = kept
		if len(old) > 0 {
			om := OwnerOldMail{Rows: old}
			var names []string
			for _, mail := range old {
				om.Sessions++
				om.Unread += mail.Unread
				if mail.OldestAgeSeconds > om.OldestAgeSeconds {
					om.OldestAgeSeconds = mail.OldestAgeSeconds
					names = append([]string(nil), mail.Session)
				} else if mail.OldestAgeSeconds == om.OldestAgeSeconds {
					names = append(names, mail.Session)
				}
			}
			om.OldestSessionList = strings.Join(names, ", ")
			inbox.OldMail = &om
		}
	}
	m.Inbox = inbox
}

// obligationMaxAgeSeconds is the mail split age in seconds: the same
// obligation rule the ask cutoff applies, so the two rules cannot differ in
// unit, boundary or meaning. A zero or unparsable value means expiry is
// disabled and nothing folds.
func obligationMaxAgeSeconds(cfg Config) float64 {
	maxAge := thread.DefaultObligationMaxAge
	if cfg.ObligationMaxAge != "" {
		if age, err := ParseDuration(cfg.ObligationMaxAge); err == nil {
			maxAge = age
		}
	}
	return maxAge.Seconds()
}

// obligationMaxAgeString is the mail split age as a duration string, for the
// JSON model and the page note; empty when expiry is disabled.
func obligationMaxAgeString(cfg Config) string {
	if d := time.Duration(obligationMaxAgeSeconds(cfg) * float64(time.Second)); d > 0 {
		return d.String()
	}
	return ""
}

// forgeItem is an unclaimed forge card flagged Needs you as an owner item: a
// pull request that asks for the owner's review is the owner's work even
// though no task claims it. Its title reads "Review #N: <the PR title>", its
// age starts at the pull request's opened time, and its link is the pull
// request's web URL, so the owner opens it in one step. A link that is not
// https is refused: the row then has no link and Details says why.
func forgeItem(c Card) OwnerItem {
	it := OwnerItem{ID: c.ID, Kind: "review_request", Title: c.Title, Summary: c.Summary, Since: c.OpenedAt}
	if it.Since.IsZero() {
		it.Since = c.Reason.Since
	}
	for _, ref := range c.Refs {
		if ref.Kind != RefForge {
			continue
		}
		if _, n, ok := strings.Cut(ref.ID, "#"); ok {
			it.Title = "Review #" + n + ": " + c.Title
		}
	}
	for _, i := range c.Items {
		for _, e := range i.Evidence {
			if e.Field == "type" && e.Value == ForgePullRequest && e.Text != "" {
				it.Title = "Review " + strings.TrimPrefix(e.Text, "PR ") + ": " + i.Title
			}
			if e.Field == FieldSummary && it.Summary == "" {
				it.Summary = Sentence(e.Text)
			}
			if e.URL != "" && it.Link == "" {
				if u, err := url.Parse(e.URL); err == nil && u.Scheme == "https" {
					it.Link = e.URL
				} else {
					it.NoAnswer = "the pull request link is not https: " + e.URL
				}
			}
		}
	}
	return it
}

// cardItem is a Needs you card as an owner item: a decision task with no
// question that names it says the options have not been sent; a bead shows
// its show-bead command; an obligation card answers through AnswerActions.
func cardItem(c Card, peers map[string]bool, seenQuestions map[string]bool) OwnerItem {
	it := OwnerItem{ID: c.ID, Kind: "task", Title: c.Title, Summary: c.Summary, Since: c.Reason.Since}
	if c.Requester != nil {
		it.From = ownerLabel(*c.Requester, nil)
	}
	for _, ref := range c.Refs {
		if ref.Kind == RefBead {
			it.Kind, it.Bead = "bead", ref.ID
			break
		}
	}
	if decisionJoined(c, seenQuestions) {
		// A question with context.bead names this task; the question's row
		// carries the decision, so the task folds into it.
		it.Kind = "hidden"
		return it
	}
	if isDecisionItem(c) {
		// The row keeps its kind and its show command: the facts are the
		// added fields, not a new kind. No answer button — the options have
		// not been sent yet.
		it.Summary = "The options have not been sent yet."
		it.NoAnswer = "no question carries this decision's options"
		for _, a := range c.Actions {
			if a.Label == "show bead" || a.Label == "show thread" {
				read := a
				it.Read = &read
				break
			}
		}
		return it
	}
	for _, i := range c.Items {
		if i.Obligation == nil {
			continue
		}
		for _, e := range i.Evidence {
			if e.Field == "kind" && e.Value != "" {
				it.Kind = e.Value
				break
			}
		}
		it.Answer, it.NoAnswer = answerItem(i.Obligation, peers, nil)
		break
	}
	for _, a := range c.Actions {
		if a.Label == "show bead" || a.Label == "show thread" {
			read := a
			it.Read = &read
			break
		}
	}
	return it
}

// seenQuestionBeads collects the bead ids open questions name in
// context.bead, the shared record of which decisions a question carries.
func seenQuestionBeads(messages map[string]Message) map[string]bool {
	seen := map[string]bool{}
	if messages == nil {
		return seen
	}
	for _, a := range humanAsks(messages) {
		if bead, ok := questionBead(a); ok {
			seen[bead] = true
		}
	}
	return seen
}

// humanAsks lists the messages that are open questions to the owner handle.
func humanAsks(messages map[string]Message) []Message {
	var out []Message
	for _, msg := range messages {
		if msg.Kind == "question" && len(msg.To) == 1 && msg.To[0] == reservedHuman {
			out = append(out, msg)
		}
	}
	return out
}

// questionBead reads context.bead of a question: the task a question asks
// about, when it names one. The value must be shaped like a bead id; any
// other text never joins a task.
func questionBead(m Message) (string, bool) {
	bead, ok := m.Context["bead"].(string)
	if !ok || !beadShapeRe.MatchString(bead) {
		return "", false
	}
	return bead, true
}

// isDecisionItem says whether a Needs you card is a decision the owner must
// make with no question that carries its options: the tracker records the
// item as a decision that waits on the human.
func isDecisionItem(c Card) bool {
	for _, i := range c.Items {
		if i.State == StateNeedsHuman && evidenceValue(i, "issue_type") == "decision" {
			return true
		}
	}
	return false
}

// decisionJoined says an open question with context.bead names the task, so
// the question's row carries the decision.
func decisionJoined(c Card, seenQuestions map[string]bool) bool {
	return cardBead(c) != "" && seenQuestions[cardBead(c)]
}

// decisionNeedsOptions is the one predicate for "this decision task is named
// by an open question". buildInbox folds the task into the question's row;
// Check does not raise decision_without_options for it.
func decisionNeedsOptions(c Card, seenQuestions map[string]bool) bool {
	return isDecisionItem(c) && !seenQuestions[cardBead(c)]
}

// cardBead is the bead id a card stands for, or "".
func cardBead(c Card) string {
	for _, ref := range c.Refs {
		if ref.Kind == RefBead {
			return ref.ID
		}
	}
	return ""
}

// answerItem is the answer commands of a request, or why it has none.
func answerItem(o *ObligationFacts, peers map[string]bool, options []string) ([]Action, string) {
	answer := AnswerActions(o, peers, options...)
	if len(answer) > 0 {
		return answer, ""
	}
	if _, blocked := answerRoute(o, peers); blocked != "" {
		return nil, blocked
	}
	return nil, "the request's kind has no answer command"
}

// obligationAnswer is the answer route of an obligation the snapshot's
// thread reduction holds open, by the rules thread open reports.
func obligationAnswer(ob *thread.Obligation) *ObligationFacts {
	kind, labels := thread.ClosingReply(ob.Opener.Kind)
	facts := &ObligationFacts{OpenerID: ob.Opener.ID, Thread: ob.Opener.Thread, OwnerSession: ob.Owner.Session, CompletionKind: kind, CompletionLabels: labels, AcknowledgeKind: format.KindStatus}
	if d, err := thread.ReplyDestination(ob); err == nil {
		facts.Destination = &Destination{Handle: d.Handle, Session: d.Session, Project: d.Project}
	}
	return facts
}
