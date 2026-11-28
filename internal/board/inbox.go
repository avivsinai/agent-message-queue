package board

import (
	"cmp"
	"slices"
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
}

// OwnerOption is one option of a decision.
type OwnerOption struct {
	Label   string   `json:"label"`
	Summary string   `json:"summary,omitempty"`
	Pros    []string `json:"pros,omitempty"`
	Cons    []string `json:"cons,omitempty"`
}

// DecidedRecord is one decision an agent recorded and the owner can still
// overrule, inside the decided window.
type DecidedRecord struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Summary     string        `json:"summary,omitempty"`
	From        string        `json:"from,omitempty"`
	Since       time.Time     `json:"since"`
	Door        string        `json:"door,omitempty"`
	Options     []OwnerOption `json:"options,omitempty"`
	Decided     string        `json:"decided,omitempty"`
	NotTaken    []string      `json:"not_taken,omitempty"`
	IfLater     string        `json:"if_later,omitempty"`
	// Overrule is the reply to the decider that reverses the decision; it
	// opens a todo the decider owes.
	Overrule *Action `json:"overrule,omitempty"`
	Read     *Action `json:"read,omitempty"`
}

// OwnerMail is the owner's unread mail in one session. It is never an item.
type OwnerMail struct {
	Session          string  `json:"session"`
	Unread           int     `json:"unread"`
	OldestAgeSeconds float64 `json:"oldest_age_seconds"`
	Read             *Action `json:"read,omitempty"`
}

// Inbox is the owner's queue: items, blocking first, then oldest first; the
// decisions recorded without the owner inside the decided window, newest
// first; then the mail lines.
type Inbox struct {
	Items   []OwnerItem     `json:"items"`
	Decided []DecidedRecord `json:"decided,omitempty"`
	Mail    []OwnerMail     `json:"mail,omitempty"`
}

// buildInbox fills m.Inbox from the one rule of what waits on the owner: the
// cards in Needs you and the open obligations owed by the owner handle. A
// question's options come from its opener's context.decision, parsed by the
// rule send applies; an invalid or absent decision gives a plain item.
// Decided records the messages that carry a taken option: a record asks for
// nothing, so it never counts as waiting.
func buildInbox(m *Model, messages map[string]Message, sessionPaths map[string]string, peers map[string]bool, now time.Time, decidedWindow time.Duration) {
	inbox := Inbox{Items: []OwnerItem{}}
	for _, col := range m.Columns {
		if col.ID != ColumnWaiting {
			continue
		}
		for _, c := range col.Cards {
			inbox.Items = append(inbox.Items, cardItem(c, peers))
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
	for _, g := range m.Gates {
		mail := OwnerMail{Session: g.Session, Unread: g.OpenCount, OldestAgeSeconds: g.OldestAgeSeconds}
		if read, ok := ReadOwnerMail(g.Session, g.OpenCount); ok {
			mail.Read = &read
		}
		inbox.Mail = append(inbox.Mail, mail)
	}
	inbox.Decided = decidedRecords(messages, now, decidedWindow, peers)
	m.Inbox = inbox
}

// decidedRecords collects the decision records inside the window, newest
// first. A record is a message of kind decision whose context.decision
// carries the option taken; an agent cannot decide a one-way door, so every
// record here names a two-way door or no door at all. The overrule command
// replies with kind todo: the answer opens an obligation the decider owes.
func decidedRecords(messages map[string]Message, now time.Time, window time.Duration, peers map[string]bool) []DecidedRecord {
	if window <= 0 {
		return nil
	}
	cutoff := now.Add(-window)
	var out []DecidedRecord
	for key, m := range messages {
		if m.Kind != format.KindDecision {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, m.Created)
		if err != nil || created.Before(cutoff) {
			continue
		}
		d, ok, err := format.ParseDecision(m.Context)
		if !ok || err != nil || d.Decided == "" || d.IsOneWayDoor() {
			continue
		}
		rec := DecidedRecord{
			ID: key, Title: m.Subject, Summary: Sentence(m.Body), Since: created,
			Door: d.Door, Decided: d.Decided, IfLater: d.IfLater,
		}
		if rec.Title == "" {
			rec.Title = "decision by " + m.From
		}
		if m.From != "" && !hasControl(m.From) {
			rec.From = m.From
		}
		for _, o := range d.Options {
			rec.Options = append(rec.Options, OwnerOption(o))
			if o.Label != d.Decided {
				rec.NotTaken = append(rec.NotTaken, o.Label)
			}
		}
		if overrule, ok := OverruleAction(m, peers); ok {
			rec.Overrule = &overrule
		}
		for _, a := range messageActions(m) {
			read := a
			rec.Read = &read
		}
		out = append(out, rec)
	}
	slices.SortStableFunc(out, func(a, b DecidedRecord) int {
		return cmp.Or(b.Since.Compare(a.Since), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// cardItem is a Needs you card as an owner item: a bead shows its show-bead
// command; an obligation card answers through AnswerActions.
func cardItem(c Card, peers map[string]bool) OwnerItem {
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

// decidedWindow is the board's decided_window, with the default when the
// configuration names none.
func decidedWindow(cfg Config) time.Duration {
	if cfg.DecidedWindow > 0 {
		return cfg.DecidedWindow
	}
	return DefaultConfig().DecidedWindow
}
