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
	for _, message := range m.Unresolved {
		commands := messageActions(message)
		if !hasControl(message.Subject) && !hasControl(message.Key) {
			commands = append(commands, newAction("Admit task (edit outcome title first)", "amq-board", "admit", "--message", message.Key, "--title", message.Subject))
		}
		if !hasControl(message.Key) {
			commands = append(commands, newAction("Dismiss unresolved ask", "amq-board", "dismiss", "--message", message.Key))
		}
		strip := pageStrip{AttachKey: message.Key, Key: message.Key, Kind: "unresolved", ID: clean(message.ID), Title: clean(message.Subject), State: "Current status unresolved", Reason: "Historical ask; no task admission or current work evidence", Owner: clean(message.From), AgeLabel: "Message recorded", CopyLabel: "Copy thread command", Actions: commands, Evidence: []pageEvidence{{Source: "amq", Field: message.Kind, Value: clean(message.Key), Text: message.Body}}, HasDetail: true}

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
	return views
}
