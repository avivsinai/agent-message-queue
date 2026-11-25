package board

import (
	"encoding/json"
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
	ProblemNewsOverdue   ProblemKind = "news_overdue"
	ProblemBoardStale    ProblemKind = "board_stale"
	ProblemSourceFailed  ProblemKind = "source_failed"
)

// Incident identifies one delivered observation. Changing rendered prose,
// an age, or a limit's example card does not create another incident.
func (p Problem) Incident() string {
	parts := []string{string(p.Kind)}
	switch p.Kind {
	case ProblemSeatStalled, ProblemSeatAtLimit:
		parts = append(parts, p.Session, p.Handle)
	case ProblemSourceFailed:
		parts = append(parts, p.Source)
	case ProblemReadyNotBegun, ProblemNotWorked:
		parts = append(parts, p.Session, p.Handle, p.CardID)
	case ProblemReplyOverdue, ProblemNewsOverdue:
		parts = append(parts, p.Request)
	case ProblemBoardStale:
	default:
		return ""
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	return string(raw)
}

// Problem is one floor problem: a seat or a card that does not move.
type Problem struct {
	Kind    ProblemKind `json:"kind"`
	Session string      `json:"session,omitempty"`
	Handle  string      `json:"handle,omitempty"`
	CardID  string      `json:"card_id,omitempty"`
	Request string      `json:"request,omitempty"`
	Source  string      `json:"source,omitempty"`
	Text    string      `json:"text"`
}

// Check lists the floor problems of a projected model. It is pure.
func Check(m Model, cfg Config, now time.Time) ([]Problem, error) {
	if err := CheckFreshness(Snapshot{PublishedAt: m.PublishedAt}, cfg, now); err != nil {
		return nil, err
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
	for _, src := range m.Sources {
		if !src.ReadFailed {
			// Partial coverage is a successful read with named limits;
			// disabled has no source error of its own.
			continue
		}
		text := fmt.Sprintf("source %s did not read: %s; the board may miss its cards", src.Name, clean(sourceGap(src).Gap))
		problems = append(problems, Problem{Kind: ProblemSourceFailed, Source: src.Name, Text: text})
	}
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
	for _, ask := range m.Asks {
		if ask.ReplyBy.IsZero() || !ask.ReplyBy.Before(now) {
			continue
		}
		wait := age(now.Sub(ask.ReplyBy))
		from := clean(participantLabel(ask.From))
		subject := cleanTitle(ask.Subject)
		problem := Problem{Kind: ProblemReplyOverdue, Request: ask.Key}
		switch {
		case !ask.Human:
			problem.Handle, problem.Session = ask.Owner.Handle, ask.Owner.Session
			owner := ask.Owner.Handle
			if ask.Owner.Session != "" {
				owner = ask.Owner.Session + "/" + owner
			}
			problem.Text = fmt.Sprintf("%s owes a reply to %s for %s: %s", clean(owner), from, wait, subject)
		case ask.Ruling:
			problem.Text = fmt.Sprintf("a ruling passed its overrule time %s ago; %s reads the thread, then closes it: %s", wait, from, subject)
		case ask.Kind == "question":
			problem.Text = fmt.Sprintf("a question to the owner passed its default time %s ago; %s reads the thread, then takes the default: %s", wait, from, subject)
		default:
			continue
		}
		problems = append(problems, problem)
	}
	for _, ask := range m.Asks {
		if ask.NewsBy.IsZero() || !ask.NewsBy.Before(now) || ask.OwnerReplied {
			continue
		}
		problem := Problem{Kind: ProblemNewsOverdue, Request: ask.Key,
			Text: fmt.Sprintf("no reply seen from %s about %s: %s", participantLabel(ask.Owner), cleanTitle(ask.Subject), age(now.Sub(ask.NewsBy)))}
		if !ask.Human {
			problem.Handle, problem.Session = ask.Owner.Handle, ask.Owner.Session
		}
		problems = append(problems, problem)
	}
	return problems, nil
}
