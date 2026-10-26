package board

// Column is one attention column. Hidden cards are not on the columns.
type Column string

const (
	ColumnWaiting    Column = "waiting_on_you"
	ColumnInFlight   Column = "in_flight"
	ColumnBlocked    Column = "blocked"
	ColumnUnclaimed  Column = "unclaimed"
	ColumnRequests   Column = "requests"
	ColumnHidden     Column = "hidden"
	ColumnHistorical Column = "historical"
)

// Columns is the display order.
var Columns = []Column{ColumnWaiting, ColumnInFlight, ColumnBlocked, ColumnUnclaimed, ColumnRequests}

// Title is the column heading.
func (c Column) Title() string {
	switch c {
	case ColumnWaiting:
		return "Needs you"
	case ColumnInFlight:
		return "Marked in progress"
	case ColumnBlocked:
		return "Blocked"
	case ColumnUnclaimed:
		return "Backlog"
	case ColumnRequests:
		return "Reply due"
	case ColumnHistorical:
		return "Historical; not reported"
	}
	return "Hidden"
}

// Place is the only place a column is decided (design 3.3). It looks at
// typed state, owner kind, and WaitsOn of the card's open items, first match
// wins, and returns the index of the item that placed the card (-1 when
// hidden). Free text never reaches this function. Waiting on you means a
// structured field needs the human's action; work the human owns and can
// move is In flight, and only human-owned blocked work waits on the human.
func Place(items []Item) (Column, int) {
	human := func(o Owner) bool { return o.Kind == OwnerHuman }
	if i := first(items, func(it Item) bool {
		if it.State == StateNeedsHuman {
			return true
		}
		for _, w := range it.WaitsOn {
			if human(w) {
				return true
			}
		}
		return human(it.Owner) && it.State == StateBlocked
	}); i >= 0 {
		return ColumnWaiting, i
	}
	if i := first(items, func(it Item) bool {
		return it.State == StateBlocked && it.Owner.Kind == OwnerNone && len(it.WaitsOn) == 0
	}); i >= 0 {
		return ColumnWaiting, i
	}
	if i := first(items, func(it Item) bool { return it.State == StateBlocked }); i >= 0 {
		return ColumnBlocked, i
	}
	if i := first(items, func(it Item) bool { return it.State == StateActive }); i >= 0 {
		return ColumnInFlight, i
	}
	if i := first(items, func(it Item) bool { return it.State == StateOpenUnowned || it.State == StateQueued }); i >= 0 {
		return ColumnUnclaimed, i
	}
	if i := first(items, func(it Item) bool { return it.State == StateRequested }); i >= 0 {
		return ColumnRequests, i
	}
	return ColumnHidden, -1
}

func first(items []Item, match func(Item) bool) int {
	for i, it := range items {
		if it.State == StateDone || it.State == StatePlanned {
			continue
		}
		if match(it) {
			return i
		}
	}
	return -1
}
