package board

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// pageRequests is the Requests view: the headline and its qualifications,
// Recent requests by addressee, then Undated; and the separate Older view.
type pageRequests struct {
	Headline, Partition     string
	Notes                   []string
	Coverage                pageGaps
	RecentCount, OlderCount int
	Groups                  []pageRequestGroup
	Undated                 []pageRequest
	Older                   []pageRequest
	OlderMore, UndatedMore  string
}

// olderShown bounds the Older view and the Undated rows; viewLimit bounds the full rows and
// repliedShown the compact rows of one addressee, so the page does not grow
// with history. --json keeps every request.
const (
	olderShown   = 200
	repliedShown = 5
)

type pageRequestGroup struct {
	ID, Addressee, Count, More string
	Rows                       []pageRequest
}

// pageRequest is one request row. Facts are observations; Opinions are
// attributed classifier labels, rendered apart from the facts. Only a Full
// row carries Details: a request with a direct reply from every scoped
// recipient stays compact, so the page does not grow with answered history.
type pageRequest struct {
	Full                                      bool
	ID, Key, MessageID, Kind, Title, From, To string
	Prefix, Summary                           string
	Age, AgeAt                                string
	Facts                                     []requestFact
	Opinions                                  []pageOpinion
	Tasks, Mentions                           []string
	Read                                      *Action
	Actions                                   []Action
}

type pageOpinion struct{ Value, Classifier, Quote, Source string }

func buildRequestsPage(m Model, now time.Time) pageRequests {
	v := m.Requests
	p := pageRequests{RecentCount: len(v.Recent), OlderCount: len(v.Older)}
	p.Headline = requestHeadline(v)
	for _, note := range requestBookkeeping(v) {
		p.Notes = append(p.Notes, clean(note))
	}
	p.Partition = "Recent is the last " + strconv.Itoa(int(RequestWindow/(24*time.Hour))) + " days by message time: " + strconv.Itoa(len(v.Recent)) + " recent, " + strconv.Itoa(len(v.Older)) + " older"
	if n := len(v.Undated); n > 0 {
		p.Partition += ", " + strconv.Itoa(n) + " undated"
	}
	p.Partition += "."
	if !v.Coverage.Complete {
		gap := "requests may be missing, and a missing reply cannot be told apart from one that was not read"
		if v.Coverage.Detail != "" {
			gap = clean(v.Coverage.Detail) + "; " + gap
		}
		p.Coverage = pageGaps{{Name: "amq-history", Status: "partly read", Gap: gap}}
	}
	named := askTasks(m.Columns)
	for i, g := range recentGroups(v) {
		group := pageRequestGroup{ID: "requests-" + strconv.Itoa(i), Addressee: clean(g.Addressee), Count: pluralize(len(g.Requests), "request")}
		full, replied := 0, 0
		for _, r := range g.Requests {
			row := requestRow(r, group.ID, named, now)
			if row.Full {
				if full++; full > viewLimit {
					continue
				}
			} else if replied++; replied > repliedShown {
				continue
			}
			group.Rows = append(group.Rows, row)
		}
		if n := len(g.Requests) - len(group.Rows); n > 0 {
			group.More = pluralize(n, "more request") + " for this addressee: amq-board --json"
		}
		p.Groups = append(p.Groups, group)
	}
	for _, r := range v.Undated[:min(len(v.Undated), olderShown)] {
		p.Undated = append(p.Undated, requestRow(r, "requests-undated", named, now))
	}
	if n := len(v.Undated) - len(p.Undated); n > 0 {
		p.UndatedMore = pluralize(n, "more undated request") + ": amq-board --json"
	}
	for _, r := range v.Older[:min(len(v.Older), olderShown)] {
		p.Older = append(p.Older, olderRow(r, now))
	}
	if n := len(v.Older) - len(p.Older); n > 0 {
		p.OlderMore = pluralize(n, "more older request") + ": amq-board --json"
	}
	return p
}

func requestRow(r Request, bay string, named map[string][]Card, now time.Time) pageRequest {
	msg := r.Message
	row := olderRow(r, now)
	sum := sha256.Sum256([]byte(msg.Key))
	row.ID = "req-" + bay + "-" + base64.RawURLEncoding.EncodeToString(sum[:12])
	row.Full = r.NoDirectReply
	for _, rec := range r.Recipients {
		row.Full = row.Full || !rec.Scoped
	}
	for _, f := range requestFacts(r) {
		row.Facts = append(row.Facts, requestFact{f.State, clean(f.Text)})
	}
	for _, o := range r.Opinions {
		po := pageOpinion{Value: requestLabelValues[o.Value], Classifier: clean(o.Classifier)}
		if o.Quote != nil {
			po.Quote = quoteText(o.Quote.Text)
			po.Source = clean(quoteSource(o, now))
		}
		row.Opinions = append(row.Opinions, po)
	}
	for _, t := range r.Tasks {
		state := "recorded " + string(t.State)
		if t.Cancelled {
			state = "owner-cancelled"
		}
		row.Tasks = append(row.Tasks, cleanTitle(t.Title)+" ("+clean(t.TaskID)+"): "+t.Binding+", "+clean(state))
	}
	for _, card := range named[msg.Key] {
		row.Mentions = append(row.Mentions, "“"+cleanTitle(card.Title)+"” ("+clean(card.ID)+")")
	}
	if !row.Full {
		return row
	}
	if !hasControl(msg.Subject) && !hasControl(msg.Key) {
		row.Actions = append(row.Actions, newOwnerAction("Admit task (edit outcome title first)", "amq-board", "admit", "--message", msg.Key, "--title", msg.Subject))
	}
	if !hasControl(msg.Key) {
		row.Actions = append(row.Actions, newOwnerAction("Dismiss this request from the board", "amq-board", "dismiss", "--message", msg.Key))
	}
	return row
}

// olderRow is the compact row of the Older view: what was asked, by whom,
// of whom, and when, with the thread command. Bodies stay in --json.
func olderRow(r Request, now time.Time) pageRequest {
	msg := r.Message
	row := pageRequest{Key: clean(msg.Key), MessageID: clean(msg.ID), Kind: clean(strings.ReplaceAll(msg.Kind, "_", " ")), Title: cleanTitle(msg.Subject), From: clean(msg.From), Age: "time unknown", Summary: clean(r.Summary)}
	if row.Title == "" {
		row.Title = "(no subject)"
	}
	row.Prefix, row.Title = commitPrefix(row.Title)
	if r.Sender != nil {
		row.From = clean(participantLabel(*r.Sender))
	}
	var to []string
	for _, rec := range r.Recipients {
		if rec.Scoped {
			to = append(to, participantLabel(rec.participant()))
		} else {
			to = append(to, rec.Handle+" (scope unknown)")
		}
	}
	row.To = clean(strings.Join(to, ", "))
	if at, ok := messageTime(msg); ok {
		row.Age, row.AgeAt = age(now.Sub(at))+" ago", at.UTC().Format(time.RFC3339)
	}
	if actions := messageActions(msg); len(actions) > 0 {
		row.Read = &actions[0]
	}
	return row
}
