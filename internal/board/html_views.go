package board

import (
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
			st.More = "+" + strconv.Itoa(v.Omitted) + " more in History"
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
