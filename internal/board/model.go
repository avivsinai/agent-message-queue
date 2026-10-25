package board

import (
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// Schema names the JSON face.
const Schema = "amq/board/v1"

// Config holds the board's display settings.
type Config struct {
	LivenessWindow time.Duration
	StaleAfter     time.Duration
}

// DefaultConfig is the configuration when no file sets one.
func DefaultConfig() Config {
	return Config{LivenessWindow: 30 * time.Minute, StaleAfter: 36 * time.Hour}
}

// NamedResult is one source's read, in source order.
type NamedResult struct {
	Name   string
	Result Result
}

// Input is everything the model is built from. Build is pure.
type Input struct {
	Results []NamedResult
	// Known maps every verified bead id (open, and closed ones resolved for
	// mentions) to its status.
	Known   map[string]string
	Config  Config
	Now     time.Time
	Version string
	Dir     string
}

// Model is the board: what every face renders.
type Model struct {
	Schema      string          `json:"schema"`
	GeneratedAt time.Time       `json:"generated_at"`
	Version     string          `json:"version"`
	Dir         string          `json:"dir"`
	Project     string          `json:"project,omitempty"`
	Sessions    []string        `json:"sessions"`
	Sources     []SourceView    `json:"sources"`
	Columns     []ColumnView    `json:"columns"`
	Gates       []Gate          `json:"operator_gates"`
	Roster      []RosterSession `json:"roster"`
	RosterNote  string          `json:"roster_note,omitempty"`
	Usage       *UsageView      `json:"usage,omitempty"`
	// Conversations are the harness sessions AMQ records tie to agents; they
	// attribute usage to workers.
	Conversations []Conversation `json:"-"`
}

// SourceView is one token of the sources strip.
type SourceView struct {
	Name       string       `json:"name"`
	Status     SourceStatus `json:"status"`
	Detail     string       `json:"detail,omitempty"`
	Partial    bool         `json:"partial"`
	Coverage   Coverage     `json:"coverage"`
	ObservedAt time.Time    `json:"observed_at"`
	Version    string       `json:"version,omitempty"`
}

// ColumnView is one attention column.
type ColumnView struct {
	ID    Column `json:"id"`
	Title string `json:"title"`
	Cards []Card `json:"cards"`
	Empty string `json:"empty,omitempty"`
}

// Card is one unit of work with all its evidence.
type Card struct {
	ID               string    `json:"id"`
	Column           Column    `json:"column"`
	Title            string    `json:"title"`
	Refs             []Ref     `json:"refs"`
	Reason           Reason    `json:"reason"`
	ReasonSource     string    `json:"reason_source"`
	SourceObservedAt time.Time `json:"source_observed_at"`
	Owners           []Owner   `json:"owners"`
	WaitsOn          []Owner   `json:"waits_on"`
	Requester        *Owner    `json:"requester,omitempty"`
	Marks            []string  `json:"marks"`
	Related          []Token   `json:"related"`
	Actions          []Action  `json:"actions"`
	Items            []Item    `json:"items"`
	UpdatedAt        time.Time `json:"updated_at"`
	stale            bool
}

// RosterSession is one session's agents. Quiet agents are folded.
type RosterSession struct {
	Session string        `json:"session"`
	Agents  []RosterAgent `json:"agents"`
	Quiet   []RosterAgent `json:"quiet"`
}

// RosterAgent is one chip.
type RosterAgent struct {
	RosterEntry
	Liveness    string  `json:"liveness,omitempty"`
	Obligations int     `json:"obligations"`
	Action      *Action `json:"action,omitempty"`
	Usage       []Usage `json:"usage,omitempty"`
	// LimitReached is set while the worker's harness account reports a
	// reached limit. It is a roster flag, never a column.
	LimitReached *usage.Limit `json:"limit_reached,omitempty"`
}

var titlePriority = map[string]int{"beads": 0, "github": 1, "amq": 2}

// Build turns source results into the board model.
func Build(in Input) Model {
	m := Model{
		Schema:      Schema,
		GeneratedAt: in.Now.UTC(),
		Version:     in.Version,
		Dir:         in.Dir,
		Sessions:    []string{},
		Gates:       []Gate{},
		Roster:      []RosterSession{},
	}
	var amqCtx *AMQContext
	var roster []RosterEntry
	observed := map[string]time.Time{}
	var items []Item
	amqStatus := StatusUnavailable
	amqDetail := ""
	for _, nr := range in.Results {
		r := nr.Result
		m.Sources = append(m.Sources, SourceView{
			Name: nr.Name, Status: r.Status, Detail: r.Detail, Partial: r.Coverage.Partial(),
			Coverage: r.Coverage, ObservedAt: r.ObservedAt, Version: r.Version,
		})
		observed[nr.Name] = r.ObservedAt
		if nr.Name == "amq" {
			amqStatus, amqDetail = r.Status, r.Detail
			if r.AMQ != nil {
				amqCtx = r.AMQ
			}
			roster = r.Roster
		}
		for _, it := range r.Items {
			it.Source = nr.Name
			items = append(items, it)
		}
	}

	sessionPaths := map[string]string{}
	peers := map[string]bool{}
	if amqCtx != nil {
		m.Project = amqCtx.Project
		for _, s := range amqCtx.Sessions {
			m.Sessions = append(m.Sessions, s.Name)
			sessionPaths[s.Name] = s.Path
		}
		for _, p := range amqCtx.Peers {
			peers[p] = true
		}
		m.Gates = append(m.Gates, amqCtx.Gates...)
		m.Conversations = amqCtx.Conversations
	}
	if amqStatus != StatusOK && len(roster) == 0 {
		m.RosterNote = "AMQ unavailable: roster, liveness, and agent mapping not shown"
		if amqDetail != "" {
			m.RosterNote += " (" + amqDetail + ")"
		}
	}

	// An agent named only by handle maps to a session when exactly one
	// session of the project has that mailbox.
	handleSessions := map[string][]string{}
	for _, e := range roster {
		handleSessions[e.Handle] = append(handleSessions[e.Handle], e.Session)
	}
	mapSession := func(o Owner) Owner {
		if o.Kind == OwnerAgent && o.Session == "" && len(handleSessions[o.Handle]) == 1 {
			o.Session = handleSessions[o.Handle][0]
		}
		return o
	}
	for i := range items {
		items[i].Owner = mapSession(items[i].Owner)
		for j := range items[i].WaitsOn {
			items[i].WaitsOn[j] = mapSession(items[i].WaitsOn[j])
		}
	}

	items = ResolveMentions(items, in.Known)
	threadSize := map[Ref]int{}
	for _, it := range items {
		if it.Thread != nil {
			threadSize[*it.Thread]++
		}
	}

	byColumn := map[Column][]Card{}
	for _, g := range Link(items) {
		col, idx := Place(g.items)
		if col == ColumnHidden {
			continue
		}
		c := buildCard(g, col, idx, in, observed, threadSize, sessionPaths, peers)
		byColumn[col] = append(byColumn[col], c)
	}

	for _, col := range Columns {
		cards := byColumn[col]
		sortCards(col, cards)
		cv := ColumnView{ID: col, Title: col.Title(), Cards: cards}
		if cv.Cards == nil {
			cv.Cards = []Card{}
			cv.Empty = emptyText(col, in.Results)
		}
		m.Columns = append(m.Columns, cv)
	}

	m.Roster = buildRoster(amqCtx, roster, items, in.Config)
	return m
}

func buildCard(g group, col Column, idx int, in Input, observed map[string]time.Time, threadSize map[Ref]int, sessionPaths map[string]string, peers map[string]bool) Card {
	placing := g.items[idx]
	c := Card{
		ID:               placing.Ref.String(),
		Column:           col,
		Reason:           placing.Reason,
		ReasonSource:     placing.Source,
		SourceObservedAt: observed[placing.Source],
		Requester:        placing.Requester,
		WaitsOn:          placing.WaitsOn,
		Items:            g.items,
		Related:          g.tokens(in.Known, threadSize),
		Marks:            []string{},
		Actions:          []Action{},
	}
	if c.WaitsOn == nil {
		c.WaitsOn = []Owner{}
	}
	if c.Related == nil {
		c.Related = []Token{}
	}
	titleRank := 99
	var newest time.Time
	owners := map[Owner]bool{}
	inferred := false
	for _, it := range g.items {
		c.Refs = append(c.Refs, it.Ref)
		rank, ok := titlePriority[it.Source]
		if !ok {
			rank = len(titlePriority)
		}
		if it.Title != "" && rank < titleRank {
			c.Title, titleRank = it.Title, rank
		}
		if it.UpdatedAt.After(newest) {
			newest = it.UpdatedAt
		}
		for _, e := range it.Evidence {
			if e.At.After(newest) {
				newest = e.At
			}
		}
		if it.Owner.Kind != OwnerNone && it.Structural && !owners[it.Owner] {
			owners[it.Owner] = true
			c.Owners = append(c.Owners, it.Owner)
		}
		if !it.Structural {
			inferred = true
		}
	}
	if c.Owners == nil {
		c.Owners = []Owner{}
	}
	c.UpdatedAt = newest
	if col == ColumnInFlight && !newest.IsZero() && in.Now.Sub(newest) > in.Config.StaleAfter {
		c.stale = true
		c.Marks = append(c.Marks, "stale")
	}
	if inferred {
		c.Marks = append(c.Marks, "inferred (owner)")
	}
	if len(c.Owners) > 1 {
		c.Marks = append(c.Marks, "owners disagree")
	}
	for _, it := range g.items {
		if it.Owner.Kind == OwnerUnmapped {
			c.Marks = append(c.Marks, "assignee not mapped: "+it.Owner.Display)
		}
	}
	c.Actions = cardActions(g.items, col, in.Known, sessionPaths, peers)
	return c
}

// maxActions is the most copy commands a card offers.
const maxActions = 3

func cardActions(items []Item, col Column, known map[string]string, sessionPaths map[string]string, peers map[string]bool) []Action {
	out := []Action{}
	add := func(a Action, ok bool) {
		if ok && len(out) < maxActions {
			out = append(out, a)
		}
	}
	for _, it := range items {
		o := it.Obligation
		if o == nil {
			continue
		}
		if it.Owner.Kind == OwnerHuman {
			for _, a := range AnswerActions(o, peers) {
				add(a, true)
			}
		}
		add(ShowThread(sessionPaths[o.OwnerSession], o.Thread))
		if it.Owner.Kind == OwnerAgent && it.Owner.Session != "" {
			add(MessageAgent(it.Owner.Session, it.Owner.Handle))
		}
	}
	for _, it := range items {
		if it.Ref.Kind == RefForge && ForgeType(it) == ForgePullRequest {
			add(OpenPR(it.Ref))
		}
	}
	for _, it := range items {
		if it.Ref.Kind != RefBead {
			continue
		}
		add(ShowBead(it.Ref.ID, known))
		if col == ColumnUnclaimed && it.State == StateOpenUnowned {
			add(ClaimBead(it.Ref.ID, known))
		}
	}
	return out
}

func sortCards(col Column, cards []Card) {
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := cards[i], cards[j]
		if col != ColumnWaiting && a.stale != b.stale {
			return a.stale
		}
		as, bs := a.Reason.Since, b.Reason.Since
		if as.IsZero() != bs.IsZero() {
			return !as.IsZero()
		}
		if !as.Equal(bs) {
			return as.Before(bs)
		}
		return a.ID < b.ID
	})
}

var emptyTeach = map[Column]string{
	ColumnWaiting:   "Nothing waits on you. Requests sent to user appear here.",
	ColumnInFlight:  "Nothing in flight.",
	ColumnBlocked:   "Nothing blocked.",
	ColumnUnclaimed: "Nothing unclaimed.",
}

// emptyText says "nothing" only when every source that feeds the column read
// completely; otherwise it names what was not assessed.
func emptyText(col Column, results []NamedResult) string {
	var gaps []string
	for _, nr := range results {
		r := nr.Result
		if !feeds(r.Feeds, col) {
			continue
		}
		// A degraded source names itself here; its error detail stays in
		// the source strip. Its coverage gap is a separate fact and shows
		// whether or not the source also degraded.
		if r.Status == StatusDegraded {
			gaps = append(gaps, nr.Name+" degraded (see sources)")
		}
		if (r.Status == StatusOK || r.Status == StatusDegraded) && r.Coverage.Partial() {
			detail := r.Coverage.Detail
			if detail == "" {
				detail = "partial"
			}
			gaps = append(gaps, nr.Name+": "+detail)
		}
	}
	if len(gaps) == 0 {
		return emptyTeach[col]
	}
	return "No items found. Not assessed: " + strings.Join(gaps, "; ") + "."
}

func feeds(cols []Column, col Column) bool {
	for _, c := range cols {
		if c == col {
			return true
		}
	}
	return false
}

func buildRoster(ctx *AMQContext, roster []RosterEntry, items []Item, cfg Config) []RosterSession {
	out := []RosterSession{}
	if ctx == nil {
		return out
	}
	owes := map[[2]string]int{}
	for _, it := range items {
		if it.Obligation != nil && it.Owner.Handle != "" {
			owes[[2]string{it.Owner.Session, it.Owner.Handle}]++
		}
	}
	for _, s := range ctx.Sessions {
		rs := RosterSession{Session: s.Name, Agents: []RosterAgent{}, Quiet: []RosterAgent{}}
		for _, e := range roster {
			if e.Session != s.Name {
				continue
			}
			a := RosterAgent{RosterEntry: e, Obligations: owes[[2]string{e.Session, e.Handle}]}
			notifierLive, recentPresence := false, false
			if !e.Human {
				notifierLive = e.PresenceSource == "notifier_live"
				recentPresence = e.PresenceStatus != "" && e.PresenceStatus != "unknown" && e.PresenceAgeSeconds < cfg.LivenessWindow.Seconds()
				a.Liveness = "quiet"
				if notifierLive {
					a.Liveness = "live"
				} else if recentPresence {
					a.Liveness = "recent"
				}
				if e.ValidHandle {
					if act, ok := MessageAgent(e.Session, e.Handle); ok {
						a.Action = &act
					}
				}
			}
			if notifierLive || recentPresence || e.UnreadCount > 0 || e.DLQCount > 0 || e.CountError != "" || a.Obligations > 0 || !e.ValidHandle {
				rs.Agents = append(rs.Agents, a)
			} else {
				rs.Quiet = append(rs.Quiet, a)
			}
		}
		rank := func(state string) int {
			switch state {
			case "live":
				return 0
			case "recent":
				return 1
			default:
				return 2
			}
		}
		sort.SliceStable(rs.Agents, func(i, j int) bool {
			if li, lj := rank(rs.Agents[i].Liveness), rank(rs.Agents[j].Liveness); li != lj {
				return li < lj
			}
			return rs.Agents[i].Handle < rs.Agents[j].Handle
		})
		out = append(out, rs)
	}
	return out
}
