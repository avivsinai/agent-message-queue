package board

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
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
		case s.Status == StatusDegraded:
			fmt.Fprintf(&b, "  %s degraded: %s\n", s.Name, clean(s.Detail))
		case s.Status == StatusUnavailable:
			fmt.Fprintf(&b, "  %s unavailable: %s\n", s.Name, clean(s.Detail))
		case s.Partial && s.Coverage.Detail != "":
			fmt.Fprintf(&b, "  %s not assessed: %s\n", s.Name, clean(s.Coverage.Detail))
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

	for _, c := range m.Columns {
		fmt.Fprintf(&b, "\n== %s (%d) ==\n", c.Title, len(c.Cards))
		if c.ID == ColumnWaiting {
			for _, g := range m.Gates {
				fmt.Fprintf(&b, "  %s: %d unread for user (oldest %s)\n", g.Session, g.OpenCount, age(seconds(g.OldestAgeSeconds)))
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
	fmt.Fprintf(&b, "\n== Unresolved historical asks (%d, outside task counts) ==\n", len(m.Unresolved))
	named := askTasks(m.Columns)
	asks := newestAsks(m.Unresolved)
	for _, message := range asks {
		fmt.Fprintf(&b, "  %s: %s\n", clean(message.Key), cleanTitle(message.Subject))
		for _, card := range named[message.Key] {
			fmt.Fprintf(&b, "    names task: %s (%s)\n", cleanTitle(card.Title), clean(card.ID))
		}
		fmt.Fprintf(&b, "    %s\n", clean(Quote([]string{"amq-board", "admit", "--message", message.Key, "--title", message.Subject})))
	}
	writeOmitted(&b, len(m.Unresolved)-len(asks), "older ask")
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

func writeCard(b *strings.Builder, c Card, now time.Time, live map[[2]string]string) {
	fmt.Fprintf(b, "  * %s\n    Task ID: %s\n", cleanTitle(c.Title), clean(c.ID))
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
			fmt.Fprintf(b, "    %s: %s\n", asks[0].Action.Label+" of the newest ask", asks[0].Action.Command)
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

// newestAsks returns at most viewLimit asks, newest first.
func newestAsks(asks []Message) []Message {
	out := append([]Message(nil), asks...)
	sort.SliceStable(out, func(i, j int) bool { return askNewer(out[i], out[j]) })
	return out[:min(len(out), viewLimit)]
}

// newestCards returns at most viewLimit cards, most recently updated first.
func newestCards(cards []Card) []Card {
	out := append([]Card(nil), cards...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out[:min(len(out), viewLimit)]
}

// splitRelated separates the unresolved asks that name a card from its other
// related records. Asks keep their newest-first order.
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

// askSummary is the one line a card shows for the unresolved asks that name it.
func askSummary(asks []Token) string {
	verb := " name"
	if len(asks) == 1 {
		verb = " names"
	}
	return pluralize(len(asks), "unresolved ask") + verb + " this task. Newest " + strings.TrimPrefix(asks[0].Basis, "open ")
}

// askTasks maps each ask key to the current cards that it names.
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
