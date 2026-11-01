package board

import (
	"sort"
	"strings"
	"time"
)

// Stage is where a card's work is in its life. It is derived for each view
// from the published facts and never stored.
type Stage string

const (
	StageBacklog    Stage = "backlog"
	StageReady      Stage = "ready"
	StageInProgress Stage = "in_progress"
	StageInReview   Stage = "in_review"
	StageBlocked    Stage = "blocked"
	StageShipped    Stage = "shipped"
)

// Stages is the Flow display order.
var Stages = []Stage{StageBacklog, StageReady, StageInProgress, StageInReview, StageBlocked, StageShipped}

// Title is the stage heading.
func (s Stage) Title() string {
	switch s {
	case StageBacklog:
		return "Backlog"
	case StageReady:
		return "Ready"
	case StageInProgress:
		return "In progress"
	case StageInReview:
		return "In review"
	case StageBlocked:
		return "Blocked"
	case StageShipped:
		return "Shipped"
	}
	return ""
}

// Flag says who must act next. A flag is never a stage: a card in any stage
// can carry one.
type Flag string

const (
	FlagNeedsYou Flag = "needs_you"
	FlagReplyDue Flag = "reply_due"
)

// CI is a pull request's checks rollup. It is shown on the card, never a
// stage.
type CI string

const (
	CIGreen   CI = "green"
	CIRed     CI = "red"
	CIRunning CI = "running"
	CINone    CI = "none"
)

// Text is the CI state in words.
func (c CI) Text() string {
	switch c {
	case CIGreen:
		return "CI green"
	case CIRed:
		return "CI red"
	case CIRunning:
		return "CI running"
	case CINone:
		return "no CI"
	}
	return ""
}

// StageView is one Flow column. CardIDs name cards of the model in display
// order; Omitted counts cards of the stage that are not listed.
type StageView struct {
	ID      Stage    `json:"id"`
	Title   string   `json:"title"`
	CardIDs []string `json:"card_ids"`
	Omitted int      `json:"omitted"`
}

// shippedShown is the most Shipped cards Flow lists; the rest stay in
// History.
const shippedShown = 12

// flowStage is the stage table, first match wins. fact is the card's
// authority fact; linked are the forge items that are the same work (for a
// forge-only card, the item itself). It reports false when the card is not
// on Flow: finished work with no recorded close time inside the window and
// no open pull request. The time is the deciding fact's since; zero means
// unknown.
func flowStage(fact Item, linked []Item, shippedAfter time.Time) (Stage, time.Time, bool) {
	var review, draft *Item
	var shipped time.Time
	for i := range linked {
		it := &linked[i]
		if it.State == StateDone {
			if t := closeTime(*it); !t.IsZero() && !t.Before(shippedAfter) && t.After(shipped) {
				shipped = t
			}
			continue
		}
		if ForgeType(*it) != ForgePullRequest {
			continue
		}
		switch {
		case !isDraft(*it) && review == nil:
			review = it
		case isDraft(*it) && draft == nil:
			draft = it
		}
	}
	if fact.State == StateDone {
		if t := closeTime(fact); !t.IsZero() && !t.Before(shippedAfter) && t.After(shipped) {
			shipped = t
		}
	}
	switch {
	case review != nil:
		return StageInReview, review.Reason.Since, true
	case fact.State == StateBlocked:
		return StageBlocked, fact.Reason.Since, true
	case fact.State == StateActive && ForgeType(fact) != ForgePullRequest:
		return StageInProgress, fact.Reason.Since, true
	case draft != nil:
		return StageInProgress, draft.Reason.Since, true
	case !shipped.IsZero():
		return StageShipped, shipped, true
	case fact.State == StateDone:
		return "", time.Time{}, false
	case fact.State == StateQueued:
		return StageReady, fact.Reason.Since, true
	}
	return StageBacklog, fact.Reason.Since, true
}

// closeTime is when finished work finished, or zero when no source records
// it. A bead's close time is its closed_at; a fact read before that field
// was kept has since = updated_at, which bd sets when it closes the bead.
// A forge item is done since it merged or closed. A Codex or Claude task
// records no close time.
func closeTime(it Item) time.Time {
	if it.State != StateDone {
		return time.Time{}
	}
	for _, e := range it.Evidence {
		if e.Field == "closed_at" && !e.At.IsZero() {
			return e.At
		}
	}
	if it.Ref.Kind == RefBead || it.Ref.Kind == RefForge {
		return it.Reason.Since
	}
	return time.Time{}
}

func isDraft(it Item) bool {
	for _, e := range it.Evidence {
		if e.Field == "isDraft" && e.Value == "true" {
			return true
		}
	}
	return false
}

func evidenceValue(it Item, field string) string {
	for _, e := range it.Evidence {
		if e.Field == field {
			return e.Value
		}
	}
	return ""
}

// ciOf reads a pull request's checks rollup.
func ciOf(pr Item) CI {
	switch evidenceValue(pr, "checks") {
	case "SUCCESS":
		return CIGreen
	case "FAILURE", "ERROR":
		return CIRed
	case "PENDING", "EXPECTED":
		return CIRunning
	}
	return CINone
}

// buildFlow places every current card, and every forge item no current task
// claims as the same work, into one stage. A forge item rides on a task card
// only through the task fact's same_work relation; a pull request that only
// names a bead stays its own card. forge holds the published forge items;
// in supplies the clock, window, bead vocabulary, and observation times.
func buildFlow(m *Model, forge []Item, in Input, observed map[string]time.Time) {
	shippedAfter := in.Now.Add(-in.Config.ShippedWindow)
	byRef := map[Ref]Item{}
	for _, it := range forge {
		byRef[it.Ref] = it
	}
	claimed := map[Ref]bool{}
	byStage := map[Stage][]*Card{}
	place := func(c *Card) {
		if !c.current || len(c.Items) == 0 {
			return
		}
		fact := c.Items[0]
		var linked []Item
		seen := map[Ref]bool{}
		for _, it := range c.Items {
			for _, r := range it.Relations {
				f, ok := byRef[r.To]
				if r.Kind != RelationSameWork || !ok || seen[r.To] {
					continue
				}
				seen[r.To] = true
				linked = append(linked, f)
			}
		}
		sort.Slice(linked, func(i, j int) bool { return linked[i].Ref.ID < linked[j].Ref.ID })
		stage, since, ok := flowStage(fact, linked, shippedAfter)
		if !ok {
			return
		}
		for _, f := range linked {
			claimed[f.Ref] = true
			c.Items = append(c.Items, f)
			c.Refs = append(c.Refs, f.Ref)
			if ForgeType(f) == ForgePullRequest {
				if a, ok := OpenPR(f.Ref); ok {
					c.Actions = append(c.Actions, a)
				}
			}
		}
		c.Stage, c.StageSince = stage, since
		for _, f := range linked {
			if f.State == StateDone || ForgeType(f) != ForgePullRequest {
				continue
			}
			if c.CI == "" {
				c.CI = ciOf(f)
			}
			if evidenceValue(f, "reviewDecision") == "CHANGES_REQUESTED" {
				c.Marks = append(c.Marks, "changes requested")
			}
			if evidenceValue(f, "mergeable") == "CONFLICTING" {
				c.Marks = append(c.Marks, "conflicting")
			}
		}
		if stage == StageInReview {
			switch fact.State {
			case StateDone:
				c.Marks = append(c.Marks, "tracker done; PR still open")
			case StateBlocked:
				c.Marks = append(c.Marks, fact.Reason.Text)
			}
		}
		c.Flags = cardFlags(*c)
		byStage[stage] = append(byStage[stage], c)
	}
	for i := range m.Columns {
		for j := range m.Columns[i].Cards {
			place(&m.Columns[i].Cards[j])
		}
	}
	for i := range m.Historical {
		place(&m.Historical[i])
	}

	m.Forge = []Card{}
	for _, it := range forge {
		if claimed[it.Ref] {
			continue
		}
		items := ResolveMentions([]Item{it}, in.Known)
		stage, since, ok := flowStage(items[0], items, shippedAfter)
		if !ok {
			continue
		}
		col, idx := Place(items)
		if idx < 0 {
			idx = 0
		}
		c := buildCard(group{items: items}, col, idx, in, observed, nil, nil, nil)
		c.Stage, c.StageSince, c.current = stage, since, true
		if ForgeType(it) == ForgePullRequest && it.State != StateDone {
			c.CI = ciOf(it)
		}
		c.Flags = cardFlags(c)
		m.Forge = append(m.Forge, c)
	}
	sort.Slice(m.Forge, func(i, j int) bool { return m.Forge[i].ID < m.Forge[j].ID })
	for i := range m.Forge {
		byStage[m.Forge[i].Stage] = append(byStage[m.Forge[i].Stage], &m.Forge[i])
	}

	m.Flow = []StageView{}
	for _, stage := range Stages {
		cards := byStage[stage]
		sortFlow(stage, cards)
		v := StageView{ID: stage, Title: stage.Title(), CardIDs: []string{}}
		for i, c := range cards {
			if stage == StageShipped && i == shippedShown {
				v.Omitted = len(cards) - i
				break
			}
			v.CardIDs = append(v.CardIDs, c.ID)
		}
		m.Flow = append(m.Flow, v)
	}
}

// cardFlags says who must act next: Needs you when an open item of the card
// waits on the human (the predicate of the Needs you column), Reply due when
// an open ask names the card's task exactly.
func cardFlags(c Card) []Flag {
	var flags []Flag
	for _, it := range c.Items {
		if it.State != StateDone && needsHuman(it) {
			flags = append(flags, FlagNeedsYou)
			break
		}
	}
	for _, a := range c.Attachments {
		if a.Anchor != "" {
			flags = append(flags, FlagReplyDue)
			break
		}
	}
	return flags
}

// sortFlow orders one stage. Shipped is newest first, so its listed cards
// are the most recent work. Elsewhere flagged cards come first; then Ready
// and Backlog by recorded priority; then oldest in stage first, with an
// unknown age last.
func sortFlow(stage Stage, cards []*Card) {
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := cards[i], cards[j]
		as, bs := a.StageSince, b.StageSince
		if stage == StageShipped && !as.Equal(bs) {
			return as.After(bs)
		}
		if fa, fb := len(a.Flags) > 0, len(b.Flags) > 0; fa != fb {
			return fa
		}
		if stage == StageReady || stage == StageBacklog {
			pa, ka := recordedPriority(*a)
			pb, kb := recordedPriority(*b)
			if ka != kb {
				return ka
			}
			if pa != pb {
				return pa < pb
			}
		}
		if as.IsZero() != bs.IsZero() {
			return !as.IsZero()
		}
		if !as.Equal(bs) {
			return as.Before(bs)
		}
		return a.ID < b.ID
	})
}

// FlowCards indexes every card a Flow stage can name.
func (m Model) FlowCards() map[string]Card {
	out := map[string]Card{}
	for _, c := range m.Historical {
		out[c.ID] = c
	}
	for _, col := range m.Columns {
		for _, c := range col.Cards {
			out[c.ID] = c
		}
	}
	for _, c := range m.Forge {
		out[c.ID] = c
	}
	return out
}

// flowSummary is what a Flow card shows on every face.
type flowSummary struct {
	Owner, PR, CI, Age, Names string
	NeedsYou, ReplyDue        bool
}

func summarizeFlow(c Card, now time.Time) flowSummary {
	s := flowSummary{Owner: "no owner", Age: "age unknown", CI: c.CI.Text()}
	if len(c.Owners) > 0 {
		var names []string
		for _, o := range c.Owners {
			names = append(names, ownerLabel(o, nil))
		}
		s.Owner = strings.Join(names, ", ")
	}
	if !c.StageSince.IsZero() {
		s.Age = age(now.Sub(c.StageSince))
	}
	for _, it := range c.Items {
		if it.Ref.Kind == RefForge && it.State != StateDone && ForgeType(it) == ForgePullRequest {
			if _, n, ok := strings.Cut(it.Ref.ID, "#"); ok {
				s.PR = "#" + n
			}
			break
		}
	}
	if c.Authority == "" {
		var beads []string
		for _, t := range c.Related {
			if t.Ref.Kind == RefBead && t.Inferred {
				beads = append(beads, t.Ref.ID)
			}
		}
		if len(beads) > 0 {
			s.Names = "names bead " + strings.Join(beads, ", ")
		}
	}
	for _, f := range c.Flags {
		s.NeedsYou = s.NeedsYou || f == FlagNeedsYou
		s.ReplyDue = s.ReplyDue || f == FlagReplyDue
	}
	return s
}

// flowLegend is the lifecycle in the reader's words.
const flowLegend = "Backlog → Ready → In progress → In review → Shipped. Blocked can happen at any point."
