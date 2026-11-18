package board

import (
	"errors"
	"fmt"
	"time"
)

// ProblemKind names one rule of Check.
type ProblemKind string

const (
	ProblemSeatStalled   ProblemKind = "seat_stalled"
	ProblemSeatAtLimit   ProblemKind = "seat_at_limit"
	ProblemReadyNotBegun ProblemKind = "ready_not_started"
	ProblemNotWorked     ProblemKind = "in_progress_not_worked"
	ProblemReplyOverdue  ProblemKind = "reply_overdue"
)

// Problem is one floor problem: a seat or a card that does not move.
type Problem struct {
	Kind    ProblemKind `json:"kind"`
	Session string      `json:"session,omitempty"`
	Handle  string      `json:"handle,omitempty"`
	CardID  string      `json:"card_id,omitempty"`
	Text    string      `json:"text"`
}

// Check lists the floor problems of a projected model. It is pure.
func Check(m Model, cfg Config, now time.Time) ([]Problem, error) {
	if m.PublishedAt.IsZero() {
		return nil, errors.New("the board has no published snapshot; run amq-board sync --init")
	}
	elapsed := now.Sub(m.PublishedAt)
	if elapsed > cfg.LivenessWindow {
		return nil, fmt.Errorf("the board snapshot is %s old; run amq-board sync first", age(elapsed))
	}
	type cardAt struct{ card *Card }
	cards := map[string]cardAt{}
	for col := range m.Columns {
		for i := range m.Columns[col].Cards {
			c := &m.Columns[col].Cards[i]
			cards[c.ID] = cardAt{card: c}
		}
	}
	problems := []Problem{}
	seen := map[string]bool{}
	add := func(st Station, kind ProblemKind, id string, text string) {
		key := string(kind) + "\x00" + st.Handle + "\x00" + id
		if st.HandleSessions > 0 {
			if seen[key] {
				return
			}
			seen[key] = true
			st.Session = ""
		}
		problems = append(problems, Problem{Kind: kind, Session: st.Session, Handle: st.Handle, CardID: id, Text: text})
	}
	for _, st := range m.Stations {
		seat := st.Handle
		if st.HandleSessions == 0 && st.Session != "" {
			seat = st.Session + "/" + seat
		}
		switch st.State {
		case StationStalled:
			add(st, ProblemSeatStalled, "", fmt.Sprintf("%s: unread mail waits and the seat shows no activity since it arrived (%s)", seat, st.StateWhy))
		case StationLimit:
			if len(st.Now) > 0 {
				first := st.Now[0]
				title := first
				if row, ok := cards[first]; ok {
					title = row.card.Title
				}
				add(st, ProblemSeatAtLimit, first, fmt.Sprintf("%s is at its plan limit and owns %d current card(s), first: %s", seat, len(st.Now), title))
			}
		case StationIdle, StationOff:
			// A free seat has no card in progress. Cards whose owner works
			// need activity evidence to count as neglected.
			hasStarted := false
			for _, id := range st.Now {
				if row, ok := cards[id]; ok && row.card.Stage == StageInProgress {
					hasStarted = true
				}
			}
			state := string(st.State)
			for _, id := range st.Now {
				row, ok := cards[id]
				if !ok {
					continue
				}
				card := row.card
				title := card.Title
				switch card.Stage {
				case StageReady:
					if !hasStarted {
						add(st, ProblemReadyNotBegun, id, fmt.Sprintf("%s is %s and has ready work that is not started: %s", seat, state, title))
					}
				case StageInProgress:
					if !st.LastActivity.IsZero() && !card.StageSince.IsZero() && now.Sub(card.StageSince) > cfg.LivenessWindow {
						add(st, ProblemNotWorked, id, fmt.Sprintf("%s is in progress for %s, and its owner %s shows no activity for %s (%s)", title, age(now.Sub(card.StageSince)), seat, age(now.Sub(st.LastActivity)), st.StateWhy))
					}
				}
			}
		}
	}
	for _, col := range m.Columns {
		for _, c := range col.Cards {
			for _, item := range c.Items {
				if item.Obligation == nil || item.Obligation.ReplyBy.IsZero() || !item.Obligation.ReplyBy.Before(now) {
					continue
				}
				requester := "unknown requester"
				if item.Requester != nil {
					requester = item.Requester.Display
				}
				problem := Problem{Kind: ProblemReplyOverdue, CardID: c.ID}
				if item.Owner.Kind == OwnerHuman {
					problem.Text = fmt.Sprintf("the default time of a question to the owner passed %s ago; %s takes the default: %s",
						age(now.Sub(item.Obligation.ReplyBy)), requester, c.Title)
				} else {
					problem.Handle = item.Owner.Handle
					problem.Session = item.Owner.Session
					problem.Text = fmt.Sprintf("%s owes a reply to %s for %s: %s",
						ownerLabel(item.Owner, nil), requester, age(now.Sub(item.Obligation.ReplyBy)), c.Title)
				}
				problems = append(problems, problem)
				break
			}
		}
	}
	return problems, nil
}
