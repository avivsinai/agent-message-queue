package board

import (
	"sort"
	"strconv"
	"strings"
)

// NextAction points to an existing card and says which recorded fact needs attention.
type NextAction struct {
	CardID string `json:"card_id"`
	Reason string `json:"reason"`
}

func selectNext(columns []ColumnView) []NextAction {
	parents := map[taskRef]bool{}
	for _, column := range columns {
		for _, card := range column.Cards {
			for _, item := range card.Items {
				if item.Source != "beads" || item.State == StateDone || item.State == StatePlanned {
					continue
				}
				for _, relation := range item.Relations {
					if !relation.Inferred && relation.Basis == "parent-child" && relation.To.Kind == RefBead {
						parents[taskRef{item.Namespace, relation.To}] = true
					}
				}
			}
		}
	}
	type candidate struct {
		card     Card
		next     NextAction
		tier     int
		priority int
		known    bool
	}
	var candidates []candidate
	for _, column := range columns {
		for _, card := range column.Cards {
			if isEpic(card) || isKnownParent(card, parents) || card.Column != ColumnWaiting && !hasCopyAction(card) {
				continue
			}
			var tier int
			var reason string
			switch card.Column {
			case ColumnWaiting:
				reason = "Needs your input"
			case ColumnRequests:
				tier, reason = 1, "Reply due"
				for _, owner := range card.WaitsOn {
					name := owner.Display
					if name == "" {
						name = owner.Handle
					}
					if name != "" {
						reason += " from " + name
						break
					}
				}
			case ColumnBlocked:
				if len(card.WaitsOn) == 0 {
					continue
				}
				tier, reason = 1, "Recorded blocker"
			case ColumnUnclaimed:
				tier, reason = 2, "Open; not started"
			default:
				continue
			}
			priority, known := recordedPriority(card)
			candidates = append(candidates, candidate{card, NextAction{card.ID, reason}, tier, priority, known})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.known != b.known {
			return a.known
		}
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		as, bs := a.card.Reason.Since, b.card.Reason.Since
		if as.IsZero() != bs.IsZero() {
			return !as.IsZero()
		}
		if !as.Equal(bs) {
			return as.Before(bs)
		}
		return a.card.ID < b.card.ID
	})
	next := make([]NextAction, 0, len(candidates))
	for _, candidate := range candidates {
		next = append(next, candidate.next)
	}
	return next
}

type taskRef struct {
	Namespace string
	Ref       Ref
}

func isKnownParent(card Card, parents map[taskRef]bool) bool {
	for _, item := range card.Items {
		if parents[taskRef{item.Namespace, item.Ref}] {
			return true
		}
	}
	return false
}

func hasCopyAction(card Card) bool {
	for _, action := range card.Actions {
		if action.IsContext() {
			return true
		}
	}
	return false
}

func isEpic(card Card) bool {
	for _, item := range card.Items {
		if item.Ref.Kind != RefBead {
			continue
		}
		for _, evidence := range item.Evidence {
			if evidence.Field == "issue_type" && evidence.Value == "epic" {
				return true
			}
		}
	}
	return false
}

func recordedPriority(card Card) (int, bool) {
	priority, known := 0, false
	for _, item := range card.Items {
		for _, evidence := range item.Evidence {
			if evidence.Field != "priority" {
				continue
			}
			number, ok := strings.CutPrefix(evidence.Value, "P")
			if !ok || number == "" || strings.Trim(number, "0123456789") != "" {
				continue
			}
			n, err := strconv.Atoi(number)
			if err == nil && (!known || n < priority) {
				priority, known = n, true
			}
		}
	}
	return priority, known
}
