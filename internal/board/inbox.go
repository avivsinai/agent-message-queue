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
	Read   *Action  `json:"read,omitempty"`
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

// Inbox is the owner's queue: items, blocking first, then oldest first; then
// the mail lines.
type Inbox struct {
	Items []OwnerItem `json:"items"`
	Mail  []OwnerMail `json:"mail,omitempty"`
}

// buildInbox fills m.Inbox from the one rule of what waits on the owner: the
// cards in Needs you and the open obligations owed by the owner handle. A
// question's options come from its opener's context.decision, parsed by the
// rule send applies; an invalid or absent decision gives a plain item.
func buildInbox(m *Model, messages map[string]Message, sessionPaths map[string]string, peers map[string]bool) {
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
		it.Answer = AnswerActions(a.answer, peers, labels...)
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
		if read, ok := ReadOwnerMail(g.Session); ok {
			mail.Read = &read
		}
		inbox.Mail = append(inbox.Mail, mail)
	}
	m.Inbox = inbox
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
		it.Answer = AnswerActions(i.Obligation, peers)
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
