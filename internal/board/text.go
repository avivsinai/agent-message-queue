package board

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// WriteText renders the model for a terminal, in the page's order: header,
// sources, Waiting on you, the other columns, roster, footer.
func WriteText(w io.Writer, m Model) error {
	var b strings.Builder
	now := m.GeneratedAt

	project := m.Project
	if project == "" {
		project = m.Dir
	}
	var counts []string
	for _, c := range m.Columns {
		counts = append(counts, fmt.Sprintf("%s %d", strings.ToLower(c.Title), len(c.Cards)))
	}
	fmt.Fprintf(&b, "%s · %d session%s · %s · generated %s\n", clean(project), len(m.Sessions), plural(len(m.Sessions)), strings.Join(counts, " · "), now.Format(time.RFC3339))

	var strip []string
	for _, s := range m.Sources {
		tok := s.Name + " " + string(s.Status)
		if s.Partial {
			tok += " (partial)"
		}
		if !s.ObservedAt.IsZero() {
			tok += " " + age(now.Sub(s.ObservedAt)) + " ago"
		}
		strip = append(strip, tok)
	}
	fmt.Fprintf(&b, "sources: %s\n", strings.Join(strip, " · "))
	for _, s := range m.Sources {
		switch {
		case s.Status == StatusDegraded || s.Status == StatusUnavailable:
			fmt.Fprintf(&b, "  %s %s: %s\n", s.Name, s.Status, sourceGap(s).Gap)
		case s.Partial && s.Coverage.Detail != "":
			fmt.Fprintf(&b, "  %s not assessed: %s\n", s.Name, clean(s.Coverage.Detail))
		}
		if s.Coverage.Scope != "" {
			fmt.Fprintf(&b, "  %s reads: %s\n", s.Name, clean(s.Coverage.Scope))
		}
	}
	writeUsage(&b, m)

	live := map[[2]string]string{}
	for _, rs := range m.Roster {
		for _, a := range append(append([]RosterAgent{}, rs.Agents...), rs.Quiet...) {
			live[[2]string{a.Session, a.Handle}] = a.Liveness
		}
	}

	if len(m.Next) > 0 {
		b.WriteString("\n== Next ==\n")
		byID := map[string]Card{}
		for _, column := range m.Columns {
			for _, card := range column.Cards {
				byID[card.ID] = card
			}
		}
		for i, next := range m.Next {
			if i == 5 {
				break
			}
			fmt.Fprintf(&b, "  %s: %s\n", clean(next.Reason), cleanTitle(byID[next.CardID].Title))
			if actions := byID[next.CardID].Actions; len(actions) > 0 {
				fmt.Fprintf(&b, "    copy command: %s\n", actions[0].Command)
			}
		}
	}

	writeFlow(&b, m, now)
	writeFloor(&b, m, now)

	for _, c := range m.Columns {
		fmt.Fprintf(&b, "\n== %s (%d) ==\n", c.Title, len(c.Cards))
		if c.ID == ColumnWaiting {
			for _, g := range m.Gates {
				fmt.Fprintf(&b, "  %s: %d unread for user (oldest %s)\n", g.Session, g.OpenCount, age(seconds(g.OldestAgeSeconds)))
			}
			if owner, _ := requestCounts(m.Requests); owner > 0 {
				fmt.Fprintf(&b, "  %s; see Requests\n", requestHeadline(m.Requests))
			}
		}
		if len(c.Cards) == 0 {
			fmt.Fprintf(&b, "  %s\n", clean(c.Empty))
		}
		for _, card := range c.Cards {
			writeCard(&b, card, now, live)
		}
	}

	b.WriteString("\n== Suggestions — AMQ quality not yet measured ==\n")
	fmt.Fprintf(&b, "  %s: %s\n", clean(m.SuggestionDiagnostic.Status), clean(m.SuggestionDiagnostic.Detail))
	for _, p := range m.Suggestions {
		fmt.Fprintf(&b, "  %s: %s\n    exact quote: %s\n    selected IDs: %s\n", clean(p.ID), clean(p.Unit.MessageKey), clean(p.Unit.Quote), strings.Join(p.Selected, ", "))
		for _, c := range p.Choices {
			fmt.Fprintf(&b, "    %s: %s %s %s\n", clean(c.ID), clean(c.Kind), clean(c.TaskID), clean(c.Role))
		}
		for _, a := range p.Commands {
			fmt.Fprintf(&b, "    %s: %s\n", clean(a.Label), clean(a.Command))
		}
	}
	fmt.Fprintf(&b, "\n== Historical tasks (%d, outside current work counts) ==\n", len(m.Historical))
	historical := newestCards(m.Historical)
	for _, card := range historical {
		writeCard(&b, card, now, live)
	}
	writeOmitted(&b, len(m.Historical)-len(historical), "older historical task")
	writeRequests(&b, m, now)
	b.WriteString("\n== Roster ==\n")
	if m.RosterNote != "" {
		fmt.Fprintf(&b, "  %s\n", clean(m.RosterNote))
	}
	for _, rs := range m.Roster {
		fmt.Fprintf(&b, "  %s:", rs.Session)
		if len(rs.Agents) == 0 {
			b.WriteString(" no live agents")
		}
		b.WriteString("\n")
		for _, a := range rs.Agents {
			fmt.Fprintf(&b, "    %s\n", clean(chip(a)))
			if u := usageLine(a, now); u != "" {
				fmt.Fprintf(&b, "      %s\n", u)
			}
		}
		if len(rs.Quiet) > 0 {
			uncounted := 0
			for _, a := range rs.Quiet {
				if !a.Counted && a.CountError == "" {
					uncounted++
				}
			}
			line := fmt.Sprintf("%d quiet", len(rs.Quiet))
			if uncounted > 0 {
				line += fmt.Sprintf(" (%d not in config.json: unread not counted)", uncounted)
			}
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}

	fmt.Fprintf(&b, "\nregenerate: amq-board · %s\n", m.Version)
	_, err := io.WriteString(w, b.String())
	return err
}

// writeFlow prints one line per Flow card under its stage: flags, title,
// owner, PR and CI, and time in stage.
func writeFlow(b *strings.Builder, m Model, now time.Time) {
	if len(m.Flow) == 0 {
		return
	}
	fmt.Fprintf(b, "\n== Flow ==\n  %s\n", flowLegend)
	cards := m.FlowCards()
	for _, v := range m.Flow {
		fmt.Fprintf(b, "  %s (%d)\n", v.Title, len(v.CardIDs)+v.Omitted)
		for _, id := range v.CardIDs {
			c := cards[id]
			f := summarizeFlow(c, now)
			parts := []string{cleanTitle(c.Title), f.Owner}
			if f.PR != "" {
				parts = append(parts, strings.TrimSpace(f.PR+" "+f.CI))
			}
			stageAge := f.Age
			if c.StageSince.IsZero() && !c.OpenedAt.IsZero() {
				stageAge = "open " + age(now.Sub(c.OpenedAt))
			}
			parts = append(parts, stageAge)
			if f.Names != "" {
				parts = append(parts, f.Names)
			}
			flags := ""
			if f.NeedsYou {
				flags += "◆ Needs you · "
			}
			fmt.Fprintf(b, "    %s%s\n", flags, clean(strings.Join(parts, " · ")))
			if c.Summary != "" {
				fmt.Fprintf(b, "      %s\n", clean(c.Summary))
			}
		}
		writeOmitted(b, v.Omitted, "older shipped card")
	}
}

// writeFloor prints one line per station under its session: state, handle,
// harness, why, current work, inbox, self-report, and limit; then the
// dormant mailboxes.
func writeFloor(b *strings.Builder, m Model, now time.Time) {
	if m.Stations == nil {
		return
	}
	states, dormant := floorCounts(m)
	if len(states) == 0 {
		states = []string{"no seat with recent evidence"}
	}
	fmt.Fprintf(b, "\n== Floor ==\n  %s\n", strings.Join(states, " · "))
	cards := m.FlowCards()
	session := ""
	for _, st := range m.Stations {
		if st.Session != session {
			session = st.Session
			fmt.Fprintf(b, "  %s\n", clean(session))
		}
		f := summarizeStation(st, cards, now)
		parts := []string{st.State.Glyph() + " " + st.State.Title(), clean(st.Handle)}
		if f.Harness != "" {
			parts = append(parts, f.Harness)
		}
		parts = append(parts, clean(st.StateWhy))
		work := "now: " + f.Now
		if f.NowStage != "" {
			work += " (" + f.NowStage + ")"
		}
		if f.NowMore != "" {
			work += " " + f.NowMore
		}
		if f.Shared != "" {
			work += " · " + f.Shared
		}
		parts = append(parts, work, "inbox "+f.Inbox, f.Wake)
		if f.Says != "" {
			parts = append(parts, "says “"+f.Says+"”")
		}
		if f.Status != "" {
			parts = append(parts, "status “"+f.Status+"”")
		}
		if f.Limit != "" {
			parts = append(parts, f.Limit)
		}
		fmt.Fprintf(b, "    %s\n", strings.Join(parts, " · "))
	}
	if dormant != "" {
		var by []string
		for _, d := range m.Dormant {
			by = append(by, clean(d.Session)+" "+strconv.Itoa(d.Count))
		}
		fmt.Fprintf(b, "  %s: %s\n", dormant, strings.Join(by, ", "))
	}
}

func writeCard(b *strings.Builder, c Card, now time.Time, live map[[2]string]string) {
	fmt.Fprintf(b, "  * %s\n", cleanTitle(c.Title))
	if c.Summary != "" {
		fmt.Fprintf(b, "    %s\n", clean(c.Summary))
	}
	fmt.Fprintf(b, "    Task ID: %s\n", clean(c.ID))
	if c.HistoryStatus != "" {
		fmt.Fprintf(b, "    History: %s\n", clean(c.HistoryStatus))
	}
	for _, binding := range c.ModelAttachments {
		fmt.Fprintf(b, "    Inferred attachment (%s), %s [%d:%d]: %s\n", clean(binding.Role), clean(binding.Unit.MessageKey), binding.Unit.Start, binding.Unit.End, clean(binding.Unit.Quote))
	}
	var refs []string
	for _, it := range c.Items {
		refs = append(refs, refLabel(it))
	}
	fmt.Fprintf(b, "    %s\n", clean(strings.Join(refs, " · ")))
	since := "since unknown"
	if !c.Reason.Since.IsZero() {
		since = age(now.Sub(c.Reason.Since)) + " ago"
	}
	checked := ""
	if !c.SourceObservedAt.IsZero() {
		checked = " · " + c.ReasonSource + " checked " + age(now.Sub(c.SourceObservedAt)) + " ago"
	}
	fmt.Fprintf(b, "    %s %s%s\n", clean(c.Reason.Text), since, checked)
	var who []string
	for _, o := range c.Owners {
		who = append(who, "owner "+ownerLabel(o, live))
	}
	if len(c.Owners) == 0 {
		who = append(who, "no owner")
	}
	for _, o := range c.WaitsOn {
		who = append(who, "waits on "+ownerLabel(o, live))
	}
	if c.Requester != nil {
		who = append(who, "requested by "+ownerLabel(*c.Requester, nil))
	}
	who = append(who, c.Marks...)
	fmt.Fprintf(b, "    %s\n", clean(strings.Join(who, " · ")))
	others, asks := splitRelated(c.Related)
	if len(others) > 0 {
		var rel []string
		for _, t := range others {
			rel = append(rel, tokenLabel(t))
		}
		fmt.Fprintf(b, "    related: %s\n", clean(strings.Join(rel, " · ")))
	}
	if len(asks) > 0 {
		fmt.Fprintf(b, "    %s\n", clean(askSummary(asks)))
		if asks[0].Action != nil {
			fmt.Fprintf(b, "    %s: %s\n", asks[0].Action.Label+" of the newest request", asks[0].Action.Command)
		}
	}
	for _, fact := range c.SourceFacts {
		fmt.Fprintf(b, "    source %s: %s; %s\n", clean(fact.Alias), clean(string(fact.Fact.State)), cleanTitle(fact.Fact.Title))
	}
	for _, alias := range c.Aliases {
		fmt.Fprintf(b, "    alias: %s\n", clean(alias))
	}
	for _, attachment := range c.Attachments {
		state := "attached"
		if attachment.Cancelled {
			state = "action cancelled by owner"
		}
		fmt.Fprintf(b, "    %s: %s\n      %s\n", state, clean(attachment.Message.Key), cleanTitle(attachment.Message.Subject))
	}
	for _, a := range c.Actions {
		fmt.Fprintf(b, "    %s: %s\n", a.Label, a.Command)
	}
}

// viewLimit bounds each growing group in the default text and HTML views.
// The views show the group's count and its newest entries without message
// bodies; --json and model.json remain the complete export.
const viewLimit = 25

// jsonPointer names where the complete records are.
const jsonPointer = "run amq-board --json for every record and message body"

func writeOmitted(b *strings.Builder, n int, noun string) {
	if n > 0 {
		fmt.Fprintf(b, "  %s not shown; %s\n", pluralize(n, noun), jsonPointer)
	}
}

// newestCards returns at most viewLimit cards, most recently updated first.
func newestCards(cards []Card) []Card {
	out := append([]Card(nil), cards...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out[:min(len(out), viewLimit)]
}

// writeRequests prints the request headline, then Recent requests by
// addressee with their observations and attributed opinions, at most
// viewLimit per addressee and no message bodies; then the Older and
// Undated counts.
func writeRequests(b *strings.Builder, m Model, now time.Time) {
	v := m.Requests
	fmt.Fprintf(b, "\n== Requests (outside task counts) ==\n")
	fmt.Fprintf(b, "  %s\n", requestHeadline(v))
	for _, note := range requestBookkeeping(v) {
		fmt.Fprintf(b, "    %s\n", clean(note))
	}
	fmt.Fprintf(b, "  recent (%d days) %d · older %d · undated %d\n", int(RequestWindow/(24*time.Hour)), len(v.Recent), len(v.Older), len(v.Undated))
	named := askTasks(m.Columns)
	for _, g := range recentGroups(v) {
		fmt.Fprintf(b, "  %s (%d)\n", clean(g.Addressee), len(g.Requests))
		shown := g.Requests[:min(len(g.Requests), viewLimit)]
		for _, r := range shown {
			writeRequest(b, r, named, now)
		}
		writeOmitted(b, len(g.Requests)-len(shown), "request")
	}
	if n := len(v.Older) + len(v.Undated); n > 0 {
		fmt.Fprintf(b, "  %s not listed; the page's Older requests view and %s\n", pluralize(n, "older or undated request"), jsonPointer)
	}
}

func writeRequest(b *strings.Builder, r Request, named map[string][]Card, now time.Time) {
	m := r.Message
	when := "time unknown"
	if at, ok := messageTime(m); ok {
		when = age(now.Sub(at)) + " ago"
	}
	from := m.From
	if r.Sender != nil {
		from = participantLabel(*r.Sender)
	}
	fmt.Fprintf(b, "    %s · %s · from %s · %s\n", clean(strings.ReplaceAll(m.Kind, "_", " ")), cleanTitle(m.Subject), clean(from), when)
	for _, fact := range requestFacts(r) {
		fmt.Fprintf(b, "      %s\n", clean(fact.Text))
	}
	for _, t := range r.Tasks {
		fmt.Fprintf(b, "      task: %s (%s) · %s · recorded %s\n", cleanTitle(t.Title), clean(t.TaskID), t.Binding, clean(string(t.State)))
	}
	for _, card := range named[m.Key] {
		fmt.Fprintf(b, "      mentions task: %s (%s)\n", cleanTitle(card.Title), clean(card.ID))
	}
	for _, o := range r.Opinions {
		line := "opinion: " + opinionText(o)
		if o.Quote != nil {
			line += ": “" + o.Quote.Text + "”"
			if src := quoteSource(o, now); src != "" {
				line += " (" + src + ")"
			}
		}
		fmt.Fprintf(b, "      %s\n", cleanTitle(line))
	}
	if actions := messageActions(m); len(actions) > 0 {
		fmt.Fprintf(b, "      %s: %s\n", actions[0].Label, actions[0].Command)
	}
}

// splitRelated separates the requests that name a card from its other
// related records. Requests keep their newest-first order.
func splitRelated(tokens []Token) (others, asks []Token) {
	for _, t := range tokens {
		if t.Ref.Kind == RefAMQMessage {
			asks = append(asks, t)
		} else {
			others = append(others, t)
		}
	}
	return others, asks
}

// askSummary is the one line a card shows for the requests that name it.
func askSummary(asks []Token) string {
	verb := " name"
	if len(asks) == 1 {
		verb = " names"
	}
	return pluralize(len(asks), "request") + verb + " this task. Newest " + asks[0].Basis
}

// askTasks maps each request key to the current cards that it names.
func askTasks(columns []ColumnView) map[string][]Card {
	out := map[string][]Card{}
	for _, column := range columns {
		for _, card := range column.Cards {
			for _, t := range card.Related {
				if t.Ref.Kind == RefAMQMessage {
					out[t.Ref.ID] = append(out[t.Ref.ID], card)
				}
			}
		}
	}
	return out
}

func refLabel(it Item) string {
	switch it.Ref.Kind {
	case RefBead:
		return "bead " + it.Ref.ID
	case RefForge:
		for _, e := range it.Evidence {
			if e.Field == "type" && e.Text != "" {
				return e.Text
			}
		}
	case RefExternal:
		return it.Source + " " + strings.TrimPrefix(it.Ref.ID, strings.TrimPrefix(it.Source, "ext:")+":")
	case RefObligation:
		kind := ""
		for _, e := range it.Evidence {
			if e.Field == "kind" {
				kind = e.Value
			}
		}
		return "amq " + kind + " -> " + ownerLabel(it.Owner, nil)
	}
	return it.Ref.String()
}

func tokenLabel(t Token) string {
	label := t.Ref.String()
	switch t.Ref.Kind {
	case RefBead:
		label = "bead " + t.Ref.ID
	case RefAMQThread:
		return t.Basis
	case RefBranch:
		if i := strings.LastIndexByte(t.Ref.ID, ':'); i >= 0 {
			label = "branch " + t.Ref.ID[i+1:]
		}
	case RefForge:
		if _, n, ok := strings.Cut(t.Ref.ID, "#"); ok {
			label = "#" + n
		}
	}
	var notes []string
	if t.Relation == string(RelationSameWork) {
		notes = append(notes, "same work")
	}
	notes = append(notes, t.Basis)
	if t.Ambiguous {
		notes = append(notes, "ambiguous")
	}
	if t.Closed {
		notes = append(notes, "closed")
	}
	return label + " (" + strings.Join(notes, ", ") + ")"
}

func ownerLabel(o Owner, live map[[2]string]string) string {
	switch o.Kind {
	case OwnerHuman:
		return "you"
	case OwnerNone:
		return "nobody"
	case OwnerUnmapped:
		return "not mapped: " + o.Display
	}
	s := o.Display
	if s == "" {
		s = o.Handle
	}
	if o.Session != "" {
		s += " in " + o.Session
		if l := live[[2]string{o.Session, o.Handle}]; l != "" {
			s += " (" + l + ")"
		}
	}
	return s
}

func chip(a RosterAgent) string {
	parts := []string{a.Handle}
	if !a.ValidHandle {
		parts = append(parts, "invalid mailbox name")
	}
	if a.Human {
		parts = append(parts, "human")
	} else if a.Liveness != "" {
		parts = append(parts, a.Liveness)
	}
	switch {
	case a.CountError != "":
		parts = append(parts, "not counted: "+a.CountError)
	case !a.Counted:
		parts = append(parts, "unread not counted")
	}
	if a.UnreadCount > 0 {
		parts = append(parts, fmt.Sprintf("%d unread (oldest %s)", a.UnreadCount, age(seconds(a.OldestUnreadAgeSeconds))))
	}
	if a.DLQCount > 0 {
		parts = append(parts, strconv.Itoa(a.DLQCount)+" in DLQ")
	}
	if a.Obligations > 0 {
		parts = append(parts, fmt.Sprintf("owes %d", a.Obligations))
	}
	return strings.Join(parts, " · ")
}

// cleanTitle bounds source display text without changing the stored title.
// Every title shares this terminal boundary, regardless of its source.
func cleanTitle(s string) string {
	const limit = 160
	runes := make([]rune, 0, limit)
	for _, r := range s {
		if len(runes) == limit {
			return string(runes[:limit-1]) + "…"
		}
		if isControl(r) {
			r = '?'
		}
		runes = append(runes, r)
	}
	return string(runes)
}

// clean makes one data value safe for a terminal line: control characters,
// including newlines and escape sequences, become "?", so source text can
// neither move the cursor nor forge a line such as a command.
func clean(s string) string {
	if !hasControl(s) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if isControl(r) {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// SentenceLimit bounds a one-sentence summary in runes.
const SentenceLimit = 120

var (
	mdComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	mdLink    = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdHeading = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t].*$`)
	mdCode    = regexp.MustCompile("`[^`\n]*`")
	mdBullet  = regexp.MustCompile(`(?m)^[ \t]*(?:[-*+]|\d+[.)])[ \t]+`)
	mdQuote   = regexp.MustCompile(`(?m)^[ \t]*>[ \t]?`)
	mdMark    = strings.NewReplacer("`", "", "**", "", "__", "", "*", "")
	// mdRefLine is a line that only points at other work ("Closes #12").
	mdRefLine = regexp.MustCompile(`(?im)^[ \t]*(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|part of|refs?|related to)[ \t:]+(?:[\w./-]*#\d+[ \t,]*(?:and[ \t]+)?)+[ \t]*$`)
	sentLabel = regexp.MustCompile(`^[A-Za-z][\w/-]*(?: [\w/-]+){0,2}:(?:\s+|$)`)
)

// Private markers that keep a code-span semicolon and a bullet join out of
// the sentence cut.
const (
	codeSemicolon = "\x01"
	bulletJoin    = "\x02"
)

// Sentence reduces prose to one plain sentence: comments, headings, markdown
// syntax, reference-only lines and a short leading label are dropped, the text
// stops at the first sentence end, and the result is cut on a word boundary at
// SentenceLimit runes.
func Sentence(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = mdComment.ReplaceAllString(text, "")
	text = mdHeading.ReplaceAllString(text, "")
	text = mdRefLine.ReplaceAllString(text, "")
	text = mdLink.ReplaceAllString(text, "$1")
	text = mdCode.ReplaceAllStringFunc(text, func(c string) string { return strings.ReplaceAll(c, ";", codeSemicolon) })
	text = mdQuote.ReplaceAllString(text, "")
	text = mdMark.Replace(text)
	var para []string
	labelled := false
	for _, p := range strings.Split(strings.TrimSpace(text), "\n\n") {
		// A label alone on its paragraph introduces the next one.
		p = strings.TrimSpace(p)
		bare := strings.TrimSpace(sentLabel.ReplaceAllString(p, ""))
		labelled = labelled || bare != p
		if bare != "" {
			para = append(para, bare)
			break
		}
	}
	if len(para) == 0 {
		return ""
	}
	text = para[0]
	// What followed a dropped label starts the sentence; other text keeps
	// its own case (iOS, npm), as does a leading URL or path.
	if lead, _, _ := strings.Cut(text, " "); labelled && !strings.Contains(lead, "://") && !strings.Contains(lead, "/") {
		if r, n := utf8.DecodeRuneInString(text); r != utf8.RuneError {
			text = string(unicode.ToUpper(r)) + text[n:]
		}
	}
	first := true
	text = mdBullet.ReplaceAllStringFunc(text, func(string) string {
		if first {
			first = false
			return ""
		}
		return bulletJoin
	})
	end := len(text)
	for _, sep := range []string{". ", "? ", "! ", ";", ".\n", "?\n", "!\n"} {
		if i := strings.Index(text, sep); i >= 0 && i < end {
			end = i
			if sep != ";" {
				end = i + 1
			}
		}
	}
	text = strings.Join(strings.Fields(text[:end]), " ")
	text = strings.ReplaceAll(text, " "+bulletJoin, "; ")
	text = strings.ReplaceAll(text, bulletJoin, "; ")
	text = strings.ReplaceAll(text, codeSemicolon, ";")
	if utf8.RuneCountInString(text) <= SentenceLimit {
		return text
	}
	cut := string([]rune(text)[:SentenceLimit-1])
	// Prefer ending at a clause boundary in the second half, so the cut
	// reads as a clause rather than a dangling word.
	for _, sep := range []string{", ", " — ", " - ", ": "} {
		if i := strings.LastIndex(cut, sep); i > len(cut)/2 {
			return cut[:i] + "…"
		}
	}
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		return cut[:i] + "…"
	}
	return cut + "…"
}
