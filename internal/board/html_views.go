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
		{ID: "requests", Title: "Requests", Description: "Historical asks with unresolved current status. These are outside task and work counts. Attach, admit, or dismiss them explicitly.", Empty: "No unresolved asks found in the available history."},
	}
	byID := map[string]pageStrip{}
	for _, col := range m.Columns {
		for _, card := range col.Cards {
			s := buildStrip(card, now, live)
			byID[card.ID] = s
			i := 1
			switch col.ID {
			case ColumnUnclaimed:
				i = 2
			case ColumnRequests:
				i = 3
			case ColumnWaiting:
				for _, item := range card.Items {
					if item.Ref.Kind == RefObligation {
						i = 3
						break
					}
				}
			}
			views[i].Strips = append(views[i].Strips, s)
		}
	}
	named := askTasks(m.Columns)
	asks := newestAsks(m.Unresolved)
	if omitted := len(m.Unresolved) - len(asks); omitted > 0 {
		views[3].Description += " Showing the newest " + strconv.Itoa(len(asks)) + " of " + pluralize(len(m.Unresolved), "ask") + "; " + jsonPointer + "."
	}
	for _, message := range asks {
		commands := messageActions(message)
		if !hasControl(message.Subject) && !hasControl(message.Key) {
			commands = append(commands, newOwnerAction("Admit task (edit outcome title first)", "amq-board", "admit", "--message", message.Key, "--title", message.Subject))
		}
		if !hasControl(message.Key) {
			commands = append(commands, newOwnerAction("Dismiss unresolved ask", "amq-board", "dismiss", "--message", message.Key))
		}
		strip := pageStrip{AttachKey: message.Key, Key: message.Key, Kind: "unresolved", ID: clean(message.ID), Title: clean(message.Subject), State: "Current status unresolved", Reason: "Historical ask; no task admission or current work evidence", Owner: clean(message.From), AgeLabel: "Message recorded", Actions: commands, Evidence: []pageEvidence{{Source: "amq", Field: message.Kind, Value: clean(message.Key)}}, HasDetail: true}
		for _, card := range named[message.Key] {
			strip.Names = append(strip.Names, "Names task “"+cleanTitle(card.Title)+"” ("+clean(card.ID)+")")
		}

		if m.ActionStates[message.Key] == "open" {
			strip.State = "Open request action"
			strip.Reason = "AMQ reports an open request action; task admission is separate"
		}
		views[3].Strips = append(views[3].Strips, strip)
	}
	for _, action := range m.Next {
		if s, ok := byID[action.CardID]; ok {
			s.NextReason = clean(action.Reason)
			views[0].Strips = append(views[0].Strips, s)
		}
	}
	coverageColumns := [][]Column{
		{ColumnWaiting, ColumnBlocked, ColumnUnclaimed, ColumnRequests},
		{ColumnWaiting, ColumnInFlight, ColumnBlocked},
		{ColumnUnclaimed},
		{ColumnWaiting, ColumnRequests},
	}
	for i := range views {
		var gaps []string
		for _, source := range m.Sources {
			if source.Status == StatusDisabled || source.Status == StatusOK && !source.Partial {
				continue
			}
			relevant := len(source.Feeds) == 0
			for _, col := range coverageColumns[i] {
				relevant = relevant || feeds(source.Feeds, col)
			}
			if relevant {
				gaps = append(gaps, clean(source.Name))
			}
		}
		if len(gaps) > 0 {
			views[i].CoverageTitle = "Incomplete coverage (" + strconv.Itoa(len(gaps)) + " sources)"
			views[i].Coverage = "Incomplete coverage: " + strings.Join(gaps, ", ") + ". Some items may be missing."
		}
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

type pageFlow struct {
	Legend, Coverage, CoverageTitle string
	Stages                          []pageStage
	NeedsYou                        []pageStrip
	Gates                           []string
	ReplyDue                        int
	Unresolved                      string
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
		f.Gates = append(f.Gates, pluralize(g.OpenCount, "unread message")+" for you in "+clean(g.Session))
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
			if s.ReplyDue {
				f.ReplyDue++
			}
		}
		if v.Omitted > 0 {
			st.More = "+" + strconv.Itoa(v.Omitted) + " older shipped"
		}
		f.Stages = append(f.Stages, st)
	}
	if n := len(m.Unresolved); n > 0 {
		f.Unresolved = pluralize(n, "unresolved ask") + " not linked to a card"
	}
	var gaps []string
	for _, source := range m.Sources {
		if source.Status == StatusDisabled || source.Status == StatusOK && !source.Partial {
			continue
		}
		gaps = append(gaps, clean(source.Name))
	}
	if len(gaps) > 0 {
		f.CoverageTitle = "Incomplete coverage (" + strconv.Itoa(len(gaps)) + " sources)"
		f.Coverage = "Incomplete coverage: " + strings.Join(gaps, ", ") + ". Some cards may be missing."
	}
	return f
}

func flowStrip(c Card, now time.Time, live map[[2]string]string) pageStrip {
	s := buildStrip(c, now, live)
	sum := summarizeFlow(c, now)
	s.Stage, s.StageID, s.StageAge = c.Stage.Title(), string(c.Stage), clean(sum.Age)
	s.PR, s.CI, s.CIText = clean(sum.PR), string(c.CI), sum.CI
	s.NamesBead = clean(sum.Names)
	s.NeedsYou, s.ReplyDue = sum.NeedsYou, sum.ReplyDue
	s.Owner = clean(sum.Owner)
	if c.Authority == "" {
		s.TaskID = ""
	}
	s.Anchor = stripArgs("flow", s).ID
	return s
}

type pageFloor struct {
	Counts                                    []pageCount
	Summary, Dormant, Coverage, CoverageTitle string
	DormantBy                                 []string
	Sessions                                  []pageFloorSession
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
	for _, source := range m.Sources {
		if source.Name != "amq" || source.Status == StatusOK && !source.Partial {
			continue
		}
		f.CoverageTitle = "Incomplete coverage (amq)"
		f.Coverage = "AMQ is " + string(source.Status)
		if source.Partial {
			f.Coverage += " with a partial read"
		}
		f.Coverage += ": some seats may be missing or show Unknown."
	}
	return f
}
