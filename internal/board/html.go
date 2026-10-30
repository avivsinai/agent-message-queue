package board

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"html/template"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// The page files are named one by one, so nothing else under web/ (product
// and design records, review captures) is ever embedded or served.
var (
	//go:embed web/page.html
	pageHTML string
	//go:embed web/page.css
	pageCSS string
	//go:embed web/page.js
	pageJS string
)

var pageTemplate = template.Must(template.New("page").Funcs(template.FuncMap{"stripArgs": stripArgs}).Parse(pageHTML))

// stripCall is one strip's template input: its unique heading id and where
// its commands sit.
type stripCall struct {
	ID string
	S  pageStrip
	// Shown contains context only. Owner corrections remain in Details.
	Shown []Action
}

func stripArgs(bay string, s pageStrip) stripCall {
	sum := sha256.Sum256([]byte(s.Key))
	c := stripCall{ID: "item-" + bay + "-" + base64.RawURLEncoding.EncodeToString(sum[:12]), S: s}
	for _, action := range s.Actions {
		if action.IsContext() {
			c.Shown = []Action{action}
			break
		}
	}
	return c
}

// pageCSP allows only the page's own stylesheet and script, by hash, so no
// markup that reached the page could run or restyle it.
var pageCSP = "default-src 'none'; script-src 'sha256-" + sha256Base64(pageJS) +
	"'; style-src 'sha256-" + sha256Base64(pageCSS) +
	"'; img-src data:; base-uri 'none'; form-action 'none'"

// liveCSP is pageCSP plus fetches back to the serving origin, which the live
// page needs to read model.json and press refresh.
var liveCSP = pageCSP + "; connect-src 'self'"

func sha256Base64(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// WriteHTML renders the model as one static page. html/template escapes
// every value for its context; the model is also embedded once as JSON for
// scripts that read it. regenerate is the command line the footer offers.
func WriteHTML(w io.Writer, m Model, regenerate string) error {
	return pageTemplate.Execute(w, buildPage(m, regenerate))
}

// WriteLiveHTML renders the page `amq-board serve` serves: the same page,
// which also reads model.json from its own origin and reloads when a newer
// model exists, and offers refresh instead of a regenerate command.
func WriteLiveHTML(w io.Writer, m Model) error {
	p := buildPage(m, "")
	p.Live, p.CSP = true, liveCSP
	return pageTemplate.Execute(w, p)
}

type pageData struct {
	Model                pageModel
	CSS                  template.CSS
	JS                   template.JS
	CSP                  string
	Project              string
	Generated            string
	Clock                string
	Sessions             string
	Fleet                string
	Sources              []pageSource
	Limits               []pageLimit
	Waiting              pageBay
	Bays                 []pageBay
	Roster               []pageSession
	RosterNote           string
	Regenerate           string
	Version              string
	Dir                  string
	Live                 bool
	TaskChoices          []pageTaskChoice
	Views                []pageView
	Suggestions          []pageSuggestion
	SuggestionDiagnostic string
}

type pageSuggestion struct {
	ID, Quote, MessageKey, Location, State string
	Choices                                []string
	Commands                               []Action
}

type pageTaskChoice struct{ ID, Title, Label string }

// pageModel is the part of the model the page script reads: publication
// identity and the attachable tasks. Message bodies and evidence stay in
// --json and model.json, so the page size does not grow with history.
type pageModel struct {
	Dir         string            `json:"dir"`
	GeneratedAt time.Time         `json:"generated_at"`
	PublishedAt time.Time         `json:"published_at,omitempty"`
	Generation  uint64            `json:"generation,omitempty"`
	Redirects   map[string]string `json:"redirects"`
	Catalogue   []pageTask        `json:"catalogue"`
}

type pageTask struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	State      State  `json:"recorded_state,omitempty"`
	Cancelled  bool   `json:"cancelled"`
	Historical bool   `json:"historical"`
}

func buildPageModel(m Model) pageModel {
	pm := pageModel{Dir: m.Dir, GeneratedAt: m.GeneratedAt, PublishedAt: m.PublishedAt, Generation: m.Generation, Redirects: m.Redirects, Catalogue: []pageTask{}}
	seen := map[string]bool{}
	for _, task := range m.Catalogue {
		seen[task.ID] = true
		pm.Catalogue = append(pm.Catalogue, pageTask{ID: task.ID, Title: task.Title, State: task.State, Cancelled: task.Cancelled, Historical: task.Historical})
	}
	cards := append([]Card(nil), m.Historical...)
	for _, column := range m.Columns {
		cards = append(cards, column.Cards...)
	}
	for _, card := range cards {
		if !seen[card.ID] {
			seen[card.ID] = true
			pm.Catalogue = append(pm.Catalogue, pageTask{ID: card.ID, Title: card.Title, Cancelled: card.Cancelled, Historical: card.Historical})
		}
	}
	return pm
}

type pageView struct {
	ID, Title, Description, Empty, Coverage, CoverageTitle string
	Strips                                                 []pageStrip
}

type pageSource struct {
	Name, Status, Age, Detail, Gap string
	Partial                        bool
}

type pageLimit struct {
	Account, Limits, Other string
	Reached                bool
}

type pageBay struct {
	ID, Title, Empty string
	Gates            []string
	Strips           []pageStrip
}

type pageStrip struct {
	Key, State, NextReason, AgeLabel string
	TaskID                           string
	AttachKey                        string
	Kind, ID, Title                  string
	Refs                             []string
	Reason, Since                    string
	SinceAt                          string
	Checked                          string
	Owner                            string
	People                           []string
	Marks                            []string
	Cocked                           bool
	Related                          []string
	AskCount, Asks                   string
	AskAction                        *Action
	Names                            []string
	Evidence                         []pageEvidence
	Actions                          []Action
	HasDetail                        bool
	ReasonField, Priority            string
}

type pageEvidence struct {
	Source, Field, Value, Text, Age, URL string
}

type pageSession struct {
	Name   string
	Agents []pageAgent
	Quiet  []string
	Note   string
}

type pageAgent struct {
	Handle, Liveness, Counts, Usage string
	Human, Invalid, Limited         bool
	Action                          *Action
	Split                           []string
}

func buildPage(m Model, regenerate string) pageData {
	now := m.GeneratedAt
	published := m.PublishedAt
	if published.IsZero() {
		published = now
	}
	project := m.Project
	if project == "" {
		project = m.Dir
	}
	p := pageData{
		Model:      buildPageModel(m),
		CSS:        template.CSS(pageCSS),
		JS:         template.JS(pageJS),
		CSP:        pageCSP,
		Project:    clean(project),
		Generated:  published.Format(time.RFC3339),
		Clock:      published.Local().Format("Jan 2, 15:04 MST"),
		Sessions:   pluralize(len(m.Sessions), "session"),
		RosterNote: clean(m.RosterNote),
		Regenerate: regenerate,
		Version:    clean(m.Version),
		Dir:        clean(m.Dir),
	}
	if hasControl(regenerate) {
		// A line with a control character would not paste as one command.
		p.Regenerate = ""
	}
	if m.Usage != nil && len(m.Usage.Fleet) > 0 {
		p.Fleet = clean(spendLine(m.Usage.Fleet, now, false))
		for _, a := range m.Usage.Accounts {
			other := ""
			if a.OtherSessions > 0 {
				other = "other sessions (" + strconv.Itoa(a.OtherSessions) + "): " + spendLine(a.Other, now, a.Harness == usage.Pi)
			}
			if a.Detail != "" {
				other = strings.TrimPrefix(other+" · "+a.Detail, " · ")
			}
			p.Limits = append(p.Limits, pageLimit{
				Account: accountName(a.Key), Limits: clean(limitsLine(a.Limits, now)),
				Other: clean(other), Reached: reachedLimit(a.Limits) != nil,
			})
		}
	}
	for _, s := range m.Sources {
		ps := pageSource{Name: clean(s.Name), Status: string(s.Status), Partial: s.Partial}
		if !s.ObservedAt.IsZero() {
			ps.Age = age(now.Sub(s.ObservedAt))
		}
		if s.Status == StatusDegraded || s.Status == StatusUnavailable {
			ps.Detail = clean(s.Detail)
		}
		if s.Partial {
			ps.Gap = clean(s.Coverage.Detail)
			if ps.Gap == "" {
				ps.Gap = "not every record assessed"
			}
		}
		p.Sources = append(p.Sources, ps)
	}

	live := map[[2]string]string{}
	for _, rs := range m.Roster {
		for _, a := range append(append([]RosterAgent{}, rs.Agents...), rs.Quiet...) {
			live[[2]string{a.Session, a.Handle}] = a.Liveness
		}
	}
	for _, c := range m.Columns {
		bay := pageBay{ID: string(c.ID), Title: c.Title, Empty: clean(c.Empty)}
		for _, card := range c.Cards {
			bay.Strips = append(bay.Strips, buildStrip(card, now, live))
		}
		if c.ID == ColumnWaiting {
			for _, g := range m.Gates {
				bay.Gates = append(bay.Gates, clean(g.Session)+": "+pluralize(g.OpenCount, "unread message")+" for user, oldest "+age(seconds(g.OldestAgeSeconds)))
			}
			p.Waiting = bay
			continue
		}
		p.Bays = append(p.Bays, bay)
	}
	for _, rs := range m.Roster {
		p.Roster = append(p.Roster, buildSession(rs, now))
	}
	p.Views = buildViews(m, now, live)
	if m.SuggestionDiagnostic.Status != "" {
		p.SuggestionDiagnostic = clean(m.SuggestionDiagnostic.Status + ": " + m.SuggestionDiagnostic.Detail)
	}
	for _, suggestion := range m.Suggestions {
		ps := pageSuggestion{ID: clean(suggestion.ID), Quote: quoteText(suggestion.Unit.Quote), MessageKey: clean(suggestion.Unit.MessageKey), Location: clean(suggestion.Unit.Field) + " bytes " + strconv.Itoa(suggestion.Unit.Start) + "–" + strconv.Itoa(suggestion.Unit.End), State: "Needs your review", Commands: suggestion.Commands}
		switch {
		case suggestion.Adopted:
			ps.State = "Adopted"
		case suggestion.Ignored:
			ps.State = "Ignored; source evidence retained"
		case suggestion.Stale:
			ps.State = "Outdated; run suggestions again before adoption"
		}
		for _, choice := range suggestion.Choices {
			switch choice.Kind {
			case "attach":
				title := choice.TaskID
				for _, candidate := range suggestion.Candidates {
					if candidate.ID == choice.TaskID {
						title = candidate.Title
						break
					}
				}
				ps.Choices = append(ps.Choices, "Attach as "+strings.ReplaceAll(choice.Role, "_", " ")+" to “"+clean(title)+"” ("+clean(choice.TaskID)+")")
			case "new":
				text := "Create an inferred task; confirm this is an independent outcome"
				if choice.Outcome != nil {
					text += ": “" + clean(choice.Outcome.Quote) + "”"
				}
				ps.Choices = append(ps.Choices, text)
			case "non_actionable":
				ps.Choices = append(ps.Choices, "No action identified in this span")
			case "unresolved":
				ps.Choices = append(ps.Choices, "No supported match; keep this span unresolved")
			default:
				ps.Choices = append(ps.Choices, clean(choice.Kind))
			}
		}
		p.Suggestions = append(p.Suggestions, ps)
	}
	seen := map[string]bool{}
	addTask := func(id, title string) {
		if id == "" || seen[id] || hasControl(id) {
			return
		}
		seen[id] = true
		title = clean(title)
		p.TaskChoices = append(p.TaskChoices, pageTaskChoice{ID: id, Title: title, Label: title + " [" + id + "]"})
	}
	for _, task := range m.Catalogue {
		addTask(task.ID, task.Title)
	}
	for _, column := range m.Columns {
		for _, card := range column.Cards {
			addTask(card.ID, card.Title)
		}
	}
	for _, card := range m.Historical {
		addTask(card.ID, card.Title)
	}
	sort.Slice(p.TaskChoices, func(i, j int) bool {
		if p.TaskChoices[i].Title != p.TaskChoices[j].Title {
			return p.TaskChoices[i].Title < p.TaskChoices[j].Title
		}
		return p.TaskChoices[i].ID < p.TaskChoices[j].ID
	})
	return p
}

func buildStrip(c Card, now time.Time, live map[[2]string]string) pageStrip {
	s := pageStrip{TaskID: c.ID, Key: c.ID, Title: clean(c.Title), Reason: clean(c.Reason.Text), ReasonField: clean(c.Reason.Field), State: c.Column.Title(), AgeLabel: "State recorded"}
	if s.Title == "" {
		s.Title = clean(c.ID)
	}
	placing := Item{Ref: Ref{Kind: c.ID}}
	if len(c.Items) > 0 {
		placing = c.Items[0]
	}
	for _, it := range c.Items {
		if it.Ref.String() == c.ID {
			placing = it
			break
		}
	}
	s.Kind, s.ID = refCell(placing)
	switch placing.Ref.Kind {
	case RefBead:
		if c.Column == ColumnUnclaimed {
			s.AgeLabel = "Tracker updated"
		}
	case RefObligation:
		s.AgeLabel = "Requested"
	}
	if c.Column == ColumnInFlight && placing.Ref.Kind != RefBead {
		s.State = "Declared work"
	}
	if placing.Source == "codex-goals" {
		s.State, s.AgeLabel = "Declared goal", "Goal updated"
	}
	if c.Historical {
		s.State = clean(c.HistoryStatus)
		if s.State == "" {
			s.State = "Historical; not reported"
		}
	}
	for _, it := range c.Items {
		s.Refs = append(s.Refs, clean(it.Source+" · "+refLabel(it)))
		for _, e := range it.Evidence {
			if e.Field == "priority" && s.Priority == "" {
				s.Priority = clean(e.Value)
			}
		}
	}
	s.Since = "unknown"
	if !c.Reason.Since.IsZero() {
		s.Since = age(now.Sub(c.Reason.Since))
		s.SinceAt = c.Reason.Since.Format(time.RFC3339)
	}
	if !c.SourceObservedAt.IsZero() {
		s.Checked = clean(c.ReasonSource) + " checked " + age(now.Sub(c.SourceObservedAt)) + " ago"
	}
	switch {
	case len(c.Owners) == 0:
		s.Owner = "no owner"
	default:
		var names []string
		for _, o := range c.Owners {
			names = append(names, ownerLabel(o, nil))
		}
		s.Owner = clean(strings.Join(names, ", "))
	}
	for _, o := range c.WaitsOn {
		if owns(c.Owners, o) {
			continue
		}
		s.People = append(s.People, clean("waits on "+ownerLabel(o, nil)))
	}
	if c.Requester != nil {
		s.People = append(s.People, clean("requested by "+ownerLabel(*c.Requester, nil)))
	}
	for _, mk := range c.Marks {
		s.Marks = append(s.Marks, clean(mk))
		if mk == "stale" || mk == "owners disagree" {
			s.Cocked = true
		}
	}
	others, asks := splitRelated(c.Related)
	for _, t := range others {
		s.Related = append(s.Related, clean(tokenLabel(t)))
	}
	if len(asks) > 0 {
		s.AskCount = pluralize(len(asks), "unresolved ask")
		s.Asks = clean(askSummary(asks))
		s.AskAction = asks[0].Action
		s.HasDetail = true
	}
	for _, alias := range c.Aliases {
		s.Related = append(s.Related, clean(alias))
	}
	var ev []pageEvidence
	type dated struct {
		at time.Time
		e  pageEvidence
	}
	var all []dated
	for _, it := range c.Items {
		for _, e := range it.Evidence {
			pe := pageEvidence{Source: clean(it.Source), Field: clean(e.Field), Value: clean(e.Value), Text: clean(e.Text), URL: httpsOnly(e.URL)}
			if !e.At.IsZero() {
				pe.Age = age(now.Sub(e.At)) + " ago"
			}
			all = append(all, dated{e.At, pe})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for _, d := range all {
		ev = append(ev, d.e)
	}
	for _, attachment := range c.Attachments {
		if attachment.Anchor != "" {
			ev = append(ev, pageEvidence{Source: "amq", Field: "linked by exact id", Value: clean(attachment.Message.Key), Text: clean(attachment.Anchor)})
			continue
		}
		status := "attached " + attachment.Message.Kind
		if attachment.Cancelled {
			status += " (owner cancelled action)"
		}
		ev = append(ev, pageEvidence{Source: "amq", Field: status, Value: clean(attachment.Message.Key), Text: cleanTitle(attachment.Message.Subject)})
	}
	for _, attachment := range c.ModelAttachments {
		ev = append(ev, pageEvidence{Source: "amq", Field: "owner-adopted " + clean(attachment.Role), Value: clean(attachment.Unit.MessageKey), Text: quoteText(attachment.Unit.Quote)})
	}
	s.Evidence = ev
	s.Actions = c.Actions

	s.HasDetail = s.HasDetail || len(s.Evidence) > 0 || len(s.Related) > 0 || len(s.Refs) > 1 || len(s.Actions) > 0
	return s
}

// quoteText preserves reading structure while making other controls visible.
// The template still escapes this plain string for its HTML context.
func quoteText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isControl(r) && r != '\n' && r != '\t' {
			quoted := strconv.QuoteRuneToASCII(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// owns reports whether o is one of the card's owners, so "waits on" is
// shown only when it names someone else.
func owns(owners []Owner, o Owner) bool {
	for _, x := range owners {
		if x == o || (x.Kind == OwnerHuman && o.Kind == OwnerHuman) {
			return true
		}
	}
	return false
}

// refCell is the strip's designator: a short kind and the id.
func refCell(it Item) (string, string) {
	switch it.Ref.Kind {
	case RefBead:
		return "bead", it.Ref.ID
	case RefForge:
		label := refLabel(it)
		if kind, n, ok := strings.Cut(label, " "); ok {
			return kind, n
		}
		return "forge", it.Ref.ID
	case RefObligation:
		kind := "request"
		for _, e := range it.Evidence {
			if e.Field == "kind" && e.Value != "" {
				kind = e.Value
			}
		}
		return "amq", clean(strings.ReplaceAll(kind, "_", " "))
	case RefExternal:
		provider := strings.TrimPrefix(it.Source, "ext:")
		return clean(provider), clean(strings.TrimPrefix(it.Ref.ID, provider+":"))
	case "codex-goal":
		id := it.Ref.ID
		if len(id) > 8 {
			id = id[:8]
		}
		return "goal", id
	}
	return clean(it.Ref.Kind), clean(it.Ref.ID)
}

func buildSession(rs RosterSession, now time.Time) pageSession {
	ps := pageSession{Name: clean(rs.Session)}
	for _, a := range rs.Agents {
		pa := pageAgent{Handle: clean(a.Handle), Liveness: a.Liveness, Human: a.Human, Invalid: !a.ValidHandle, Action: a.Action, Limited: a.LimitReached != nil}
		if a.Human {
			pa.Liveness = "human"
		}
		var counts []string
		switch {
		case a.CountError != "":
			counts = append(counts, "not counted: "+a.CountError)
		case !a.Counted:
			counts = append(counts, "unread not counted")
		case a.UnreadCount > 0:
			counts = append(counts, strconv.Itoa(a.UnreadCount)+" unread · oldest "+age(seconds(a.OldestUnreadAgeSeconds)))
		}
		if a.DLQCount > 0 {
			counts = append(counts, strconv.Itoa(a.DLQCount)+" in DLQ")
		}
		if a.Obligations > 0 {
			counts = append(counts, "owes "+strconv.Itoa(a.Obligations))
		}
		pa.Counts = clean(strings.Join(counts, " · "))
		pa.Usage = clean(usageLine(a, now))
		for _, u := range a.Usage {
			if u.Unknown != "" {
				continue
			}
			pa.Split = append(pa.Split, windowName(u.Window)+": "+si(u.In)+" in, "+si(u.CacheRead)+" cache read, "+si(u.CacheWrite)+" cache write, "+si(u.Out)+" out")
		}
		ps.Agents = append(ps.Agents, pa)
	}
	for _, a := range rs.Quiet {
		ps.Quiet = append(ps.Quiet, clean(a.Handle))
	}
	uncounted := 0
	for _, a := range rs.Quiet {
		if !a.Counted && a.CountError == "" {
			uncounted++
		}
	}
	if uncounted > 0 {
		ps.Note = strconv.Itoa(uncounted) + " not in config.json: unread not counted"
	}
	return ps
}

// forgeHosts are the hosts of the configured forges. GitHub is the one
// forge the board reads, always at github.com.
var forgeHosts = map[string]bool{"github.com": true}

// httpsOnly keeps a link only when the URL parses as https on a forge host,
// with no credentials or port; anything else is shown as text, never as an
// anchor.
func httpsOnly(u string) string {
	p, err := url.Parse(u)
	if err != nil || hasControl(u) || p.Scheme != "https" || p.User != nil || p.Port() != "" || !forgeHosts[p.Host] {
		return ""
	}
	return u
}

// pluralize prints "n noun" or "n nouns".
func pluralize(n int, noun string) string {
	return strconv.Itoa(n) + " " + noun + plural(n)
}
