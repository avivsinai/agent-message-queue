package board

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

func buildViews(m Model, now time.Time, live map[[2]string]string) []pageView {
	views := []pageView{
		{ID: "next", Title: "Next", Description: "Useful next actions from the available records.", Empty: "No next actions identified in the available data. Explore Backlog or Requests."},
		{ID: "work", Title: "Work", Description: "Declared work and recorded blockers. Tracker state does not prove a worker is running.", Empty: "No work is marked started or blocked in the available data."},
		{ID: "backlog", Title: "Backlog", Description: "Open work that has not started. Assignment alone does not mean work is underway.", Empty: "No queued work found in the available data."},
	}
	byID := map[string]pageStrip{}
	for _, col := range m.Columns {
		for _, card := range col.Cards {
			s := buildStrip(card, now, live)
			byID[card.ID] = s
			i := 1
			if col.ID == ColumnUnclaimed || col.ID == ColumnRequests {
				i = 2
			}
			views[i].Strips = append(views[i].Strips, s)
		}
	}
	for _, action := range m.Next {
		if s, ok := byID[action.CardID]; ok {
			// A Next reason is the tier of the card's column, which the
			// state word already says; a summary takes its line.
			s.Why = ""
			if s.Summary == "" {
				s.Why = clean(action.Reason)
			}
			views[0].Strips = append(views[0].Strips, s)
		}
	}
	coverageColumns := [][]Column{
		{ColumnWaiting, ColumnBlocked, ColumnUnclaimed, ColumnRequests},
		{ColumnWaiting, ColumnInFlight, ColumnBlocked},
		{ColumnUnclaimed},
	}
	for i := range views {
		views[i].Coverage = coverageGaps(m.Sources, func(source SourceView) bool {
			relevant := len(source.Feeds) == 0
			for _, col := range coverageColumns[i] {
				relevant = relevant || feeds(source.Feeds, col)
			}
			return relevant
		})
	}
	history := pageView{ID: "history", Title: "History", Description: "Completed, cancelled, and no longer reported tasks. Their evidence remains available; they are outside current work and next actions.", Empty: "No historical tasks recorded."}
	historical := newestCards(m.Historical)
	if omitted := len(m.Historical) - len(historical); omitted > 0 {
		history.Description += " Showing the " + strconv.Itoa(len(historical)) + " most recently updated of " + pluralize(len(m.Historical), "task") + "; " + jsonPointer + "."
	}
	for _, card := range historical {
		history.Strips = append(history.Strips, buildStrip(card, now, live))
	}
	return append(views, history)
}

// coverageGaps lists the sources a view rests on that were not fully read.
func coverageGaps(sources []SourceView, relevant func(SourceView) bool) pageGaps {
	var gaps pageGaps
	for _, source := range sources {
		if source.Status == StatusDisabled || source.Status == StatusOK && !source.Partial || !relevant(source) {
			continue
		}
		gaps = append(gaps, sourceGap(source))
	}
	return gaps
}

type pageFlow struct {
	Legend   string
	Coverage pageGaps
	Stages   []pageStage
	NeedsYou []pageStrip
	// Gates are the unread messages for the owner, one line per session
	// with its oldest age, kept apart from the cards.
	Gates []string
	// Requests is the count of requests to the owner without a direct
	// reply, linked to the Requests view; empty when there is none.
	Requests string
}

type pageStage struct {
	ID, Title, Empty, More string
	Count                  int
	Cards                  []pageStrip
}

var stageEmpty = map[Stage]string{
	StageBacklog:    "No open work without an owner.",
	StageReady:      "Nothing is assigned and waiting to start.",
	StageInProgress: "No work is marked started. Tracker state does not prove a running worker.",
	StageInReview:   "No open pull request is ready for review.",
	StageBlocked:    "Nothing blocked.",
	StageShipped:    "Nothing finished recently. Older work is in History.",
}

// buildFlowPage renders Flow: one column per stage, plus the attention strip
// of cards that need the owner and the unread messages for the owner.
func buildFlowPage(m Model, now time.Time, live map[[2]string]string) pageFlow {
	f := pageFlow{Legend: flowLegend}
	for _, g := range m.Gates {
		f.Gates = append(f.Gates, clean(g.Session)+": "+pluralize(g.OpenCount, "unread message")+", oldest "+age(seconds(g.OldestAgeSeconds)))
	}
	cards := m.FlowCards()
	for _, v := range m.Flow {
		st := pageStage{ID: string(v.ID), Title: v.Title, Count: len(v.CardIDs) + v.Omitted, Empty: stageEmpty[v.ID]}
		for _, id := range v.CardIDs {
			c := cards[id]
			s := flowStrip(c, now, live)
			st.Cards = append(st.Cards, s)
			if s.NeedsYou {
				f.NeedsYou = append(f.NeedsYou, s)
			}
		}
		if v.Omitted > 0 {
			st.More = "+" + strconv.Itoa(v.Omitted) + " older shipped"
		}
		f.Stages = append(f.Stages, st)
	}
	if owner, _ := requestCounts(m.Requests); owner > 0 {
		f.Requests = requestHeadline(m.Requests)
	}
	f.Coverage = coverageGaps(m.Sources, func(SourceView) bool { return true })
	return f
}

func flowStrip(c Card, now time.Time, live map[[2]string]string) pageStrip {
	s := buildStrip(c, now, live)
	sum := summarizeFlow(c, now)
	s.Stage, s.StageID, s.StageAge = c.Stage.Title(), string(c.Stage), clean(sum.Age)
	s.PR, s.CI, s.CIText = clean(sum.PR), string(c.CI), sum.CI
	s.NamesBead = clean(sum.Names)
	s.NeedsYou = sum.NeedsYou
	s.Owner = clean(sum.Owner)
	if !c.OpenedAt.IsZero() {
		s.Opened = "open " + age(now.Sub(c.OpenedAt))
	}
	s.CardAge = s.StageAge
	if sum.Age == "age unknown" && s.Opened != "" {
		s.CardAge = s.Opened
	}
	switch {
	case s.SinceAt != "":
		s.Waited = s.Since + " ago"
	case sum.Age != "age unknown":
		s.Waited = s.StageAge + " in stage"
	case s.Opened != "":
		s.Waited = s.Opened
	default:
		s.Waited = "age unknown"
	}
	if c.Authority == "" {
		s.TaskID = ""
	}
	s.Anchor = stripArgs("flow", s).ID
	return s
}

type pageFloor struct {
	Counts           []pageCount
	Summary, Dormant string
	Coverage         pageGaps
	DormantBy        []string
	Sessions         []pageFloorSession
}

type pageCount struct{ State, Glyph, Text string }

type pageFloorSession struct {
	Name, Count string
	Stations    []pageStation
}

type pageStation struct {
	ID, Key, State, Word, Glyph, Why                string
	Handle, Session, Harness                        string
	Now, NowStage, NowMore, Shared                  string
	Says, Status, Inbox, Activity, Wake, WakeReason string
	Foot, ActivitySource                            string
	WakePresence                                    bool
	Limit, LimitNote, Spend                         string
	Work                                            []string
	SharedN                                         int
	Action                                          *Action
}

// buildFloorPage renders the Floor: one plate per station under its
// session, the state counts, and the dormant mailboxes.
func buildFloorPage(m Model, now time.Time) pageFloor {
	states, dormant := floorCounts(m)
	f := pageFloor{Summary: "No seat has recent evidence.", Dormant: dormant}
	if len(states) > 0 {
		f.Summary = "Agent seats with recent evidence, by session."
	}
	n := map[StationState]int{}
	for _, st := range m.Stations {
		n[st.State]++
	}
	for _, state := range StationStates {
		if n[state] > 0 {
			f.Counts = append(f.Counts, pageCount{State: string(state), Glyph: state.Glyph(), Text: strconv.Itoa(n[state]) + " " + strings.ToLower(state.Title())})
		}
	}
	for _, d := range m.Dormant {
		f.DormantBy = append(f.DormantBy, clean(d.Session)+": "+strconv.Itoa(d.Count))
	}
	cards := m.FlowCards()
	for _, st := range m.Stations {
		if n := len(f.Sessions); n == 0 || f.Sessions[n-1].Name != clean(st.Session) {
			f.Sessions = append(f.Sessions, pageFloorSession{Name: clean(st.Session)})
		}
		sum := summarizeStation(st, cards, now)
		key := st.Session + "/" + st.Handle
		hash := sha256.Sum256([]byte(key))
		ps := pageStation{
			ID: "station-" + base64.RawURLEncoding.EncodeToString(hash[:12]), Key: clean(key),
			State: string(st.State), Word: st.State.Title(), Glyph: st.State.Glyph(), Why: clean(st.StateWhy),
			Handle: clean(st.Handle), Session: clean(st.Session), Harness: sum.Harness,
			Now: sum.Now, NowStage: sum.NowStage, NowMore: sum.NowMore, Shared: sum.Shared,
			Says: sum.Says, Status: sum.Status, Inbox: sum.Inbox, Activity: sum.Activity, Wake: sum.Wake,
			WakeReason: clean(st.Wake.Reason), LimitNote: sum.Limit, Spend: sum.Spend,
		}
		ps.SharedN = st.HandleSessions
		ps.ActivitySource = st.ActivitySource
		ps.WakePresence = st.Wake.Live && st.ActivitySource != "usage"
		ps.Foot = sum.Wake
		if st.State != StationWorking {
			ps.Foot += " · " + sum.Activity
		}
		if st.Limit != nil {
			ps.Limit = clean(limitWhy(*st.Limit, now))
		}
		for _, id := range st.Now {
			c := cards[id]
			title := cleanTitle(c.Title)
			if title == "" {
				title = clean(id)
			}
			ps.Work = append(ps.Work, c.Stage.Title()+": "+title)
		}
		if a, ok := MessageAgent(st.Session, st.Handle); ok {
			ps.Action = &a
		}
		last := &f.Sessions[len(f.Sessions)-1]
		last.Stations = append(last.Stations, ps)
	}
	for i := range f.Sessions {
		f.Sessions[i].Count = pluralize(len(f.Sessions[i].Stations), "seat")
	}
	f.Coverage = coverageGaps(m.Sources, func(source SourceView) bool { return source.Name == "amq" })
	return f
}
