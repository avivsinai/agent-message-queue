package board

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// pageInbox is the Inbox view: the headline sentence, the first viewLimit
// owner items in the model's order (blocking first, then oldest), and the
// owner's unread mail. More counts the items not shown, so the page does not
// grow with history; --json keeps every item.
type pageInbox struct {
	Headline, More string
	Count          int
	Items          []pageInboxItem
	Mail           []pageMail
	// OldMail is the unread mail older than the mail split age, folded to
	// one line; MaxAge names that age.
	OldMail *pageOldMail
	MaxAge  string
	// shown holds the anchors of the rendered items.
	shown map[string]bool
}

// pageOldMail is the folded line for unread mail older than the split age;
// Rows holds one row per old session with its read command.
type pageOldMail struct {
	Sessions, Unread int
	Oldest           string
	Rows             []pageOldMailRow
}

// pageOldMailRow is one old session's folded mail line.
type pageOldMailRow struct {
	Session, Oldest string
	Read            *Action
}

type pageInboxItem struct {
	ID, Prefix, Title, Summary string
	Kind, From                 string
	Age, AgeAt                 string
	ReplyBy, ReplyByAt         string
	Overdue, Blocking          bool
	// Link is the URL the owner acts through (a pull request's web page);
	// the row shows "Open pull request" instead of copy buttons.
	Link    string
	Options []pageOption
	IfLater string
	// Buttons are the visible copy actions: one per option, the recommended
	// first, then the free answer; a request without answers shows its read
	// command instead.
	Buttons  []pageButton
	NoAnswer string
	// Commands are every command of the item as selectable fields, so a
	// failed copy has a field to select.
	Commands []Action
}

type pageOption struct {
	Label, Summary string
	Recommended    bool
	Pros, Cons     []string
}

type pageButton struct{ Label, Command string }

type pageMail struct {
	ID, Session, Counts string
	Read                *Action
}

// inboxAnchor is the element id of an owner item on the page, which the Flow
// strip links to. Item ids carry slashes and source text, so they are hashed.
func inboxAnchor(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "inbox-" + base64.RawURLEncoding.EncodeToString(sum[:12])
}

func buildInboxPage(inbox Inbox, now time.Time) pageInbox {
	p := pageInbox{Count: len(inbox.Items), Headline: "Nothing waits on you.", shown: map[string]bool{}}
	blocking := 0
	for i, it := range inbox.Items {
		if it.Blocking {
			blocking++
		}
		if i < viewLimit {
			item := buildInboxItem(it, now)
			p.shown[item.ID] = true
			p.Items = append(p.Items, item)
		}
	}
	if n := len(inbox.Items) - len(p.Items); n > 0 {
		p.More = omittedLine(n, "item")
	}
	if n := len(inbox.Items); n > 0 {
		p.Headline = pluralize(n, "item") + map[bool]string{true: " waits", false: " wait"}[n == 1] + " on you"
		if blocking > 0 {
			p.Headline += ", " + strconv.Itoa(blocking) + map[bool]string{true: " blocks", false: " block"}[blocking == 1] + " work"
		}
		p.Headline += "."
	}
	for _, mail := range inbox.Mail {
		pm := pageMail{ID: inboxAnchor("mail/" + mail.Session), Session: clean(mail.Session), Counts: pluralize(mail.Unread, "unread message") + ", oldest " + age(seconds(mail.OldestAgeSeconds)), Read: mail.Read}
		p.Mail = append(p.Mail, pm)
	}
	if inbox.OldMail != nil {
		om := &pageOldMail{
			Sessions: inbox.OldMail.Sessions,
			Unread:   inbox.OldMail.Unread,
			Oldest:   age(seconds(inbox.OldMail.OldestAgeSeconds)),
		}
		for _, mail := range inbox.OldMail.Rows {
			row := pageOldMailRow{Session: clean(mail.Session), Oldest: age(seconds(mail.OldestAgeSeconds)), Read: mail.Read}
			om.Rows = append(om.Rows, row)
		}
		p.OldMail = om
	}
	p.MaxAge = inbox.MaxAge
	return p
}

// link is the fragment a Needs you tile links to: the item's row when the
// Inbox view shows it, else the Inbox view.
func (p pageInbox) link(id string) string {
	if anchor := inboxAnchor(id); p.shown[anchor] {
		return anchor
	}
	return "view-inbox"
}

func buildInboxItem(it OwnerItem, now time.Time) pageInboxItem {
	pi := pageInboxItem{ID: inboxAnchor(it.ID), Summary: clean(it.Summary), From: clean(it.From), Blocking: it.Blocking, IfLater: clean(it.IfLater), NoAnswer: clean(it.NoAnswer)}
	pi.Prefix, pi.Title = commitPrefix(cleanTitle(it.Title))
	if pi.Title == "" {
		pi.Title = clean(it.ID)
	}
	pi.Kind = kindWord(clean(it.Kind), false)
	if it.Bead != "" {
		pi.Kind = "bead " + clean(it.Bead)
	}
	pi.Age = "age unknown"
	if !it.Since.IsZero() {
		pi.Age, pi.AgeAt = age(now.Sub(it.Since))+" ago", it.Since.Format(time.RFC3339)
	}
	if it.ReplyBy != nil {
		pi.ReplyBy, pi.ReplyByAt = it.ReplyBy.Local().Format("Jan 2, 15:04 MST"), it.ReplyBy.Format(time.RFC3339)
		pi.Overdue = it.ReplyBy.Before(now)
	}
	answers := map[string]Action{}
	var free, acknowledge []Action
	for _, a := range it.Answer {
		switch {
		case strings.HasPrefix(a.Label, "answer "):
			answers[a.Label] = a
		case strings.HasPrefix(a.Label, "complete: "):
			free = append(free, a)
		default:
			acknowledge = append(acknowledge, a)
		}
	}
	// The recommended option leads, then the others in the question's order.
	var ordered []OwnerOption
	for _, o := range it.Options {
		if o.Label == it.Recommended {
			ordered = append([]OwnerOption{o}, ordered...)
		} else {
			ordered = append(ordered, o)
		}
	}
	for _, o := range it.Options {
		po := pageOption{Label: clean(o.Label), Summary: clean(o.Summary), Recommended: o.Label == it.Recommended}
		for _, s := range o.Pros {
			po.Pros = append(po.Pros, clean(s))
		}
		for _, s := range o.Cons {
			po.Cons = append(po.Cons, clean(s))
		}
		pi.Options = append(pi.Options, po)
	}
	for _, o := range ordered {
		if a, ok := answers["answer "+o.Label]; ok {
			pi.Buttons = append(pi.Buttons, pageButton{Label: "Copy answer: " + clean(o.Label), Command: a.Command})
			pi.Commands = append(pi.Commands, a)
		}
	}
	for _, a := range free {
		label := "Copy answer"
		if len(it.Options) > 0 {
			label = "Copy free answer"
		}
		pi.Buttons = append(pi.Buttons, pageButton{Label: label, Command: a.Command})
		pi.Commands = append(pi.Commands, a)
	}
	pi.Commands = append(pi.Commands, acknowledge...)
	if it.Link != "" {
		pi.Link = it.Link
	}
	if it.Read != nil {
		if len(pi.Buttons) == 0 && pi.Link == "" {
			pi.Buttons = append(pi.Buttons, pageButton{Label: "Copy " + clean(it.Read.Label), Command: it.Read.Command})
		}
		pi.Commands = append(pi.Commands, *it.Read)
	}
	return pi
}
