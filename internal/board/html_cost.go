package board

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// pageCost is the Cost view: the fleet's list-price sentence, the spend per
// model and per seat over the longest window, and each account's limits.
// Every figure is the text view's wording of the published usage view.
type pageCost struct {
	Headline      string
	Notes         []string
	Window        string
	Models, Seats []pageCostRow
	Limits        []pageCostLimit
	ModelsEmpty   string
	SeatsEmpty    string
	HasUsage      bool
}

// pageCostRow is one table row. Meta is a seat row's session and harness
// line. Share is the row's part of its table's priced total, empty for a row
// with no price.
type pageCostRow struct {
	Name, Meta, Harness, Cost, Share string
	Fresh, Cached, Output            string
	Cost5h, Context, CacheShare      string
	Other                            bool
}

// pageCostLimit is one account and its limits, one line each.
type pageCostLimit struct {
	Account string
	Lines   []pageLimitLine
}

type pageLimitLine struct {
	Text    string
	Reached bool
}

func buildCostPage(u *UsageView, now time.Time) pageCost {
	p := pageCost{Headline: "No token usage is recorded in this snapshot.", Window: windowName(usage.Window7d)}
	if u == nil || len(u.Fleet) == 0 {
		return p
	}
	p.HasUsage = true
	longest := u.Fleet[len(u.Fleet)-1]
	p.Window = clean(windowName(longest.Window))
	var figures []string
	priced := false
	for _, w := range u.Fleet {
		priced = priced || w.CostUSD != nil || w.Unknown != ""
		// The unpriced tokens are named once, under the sentence.
		w.Unpriced = 0
		figures = append(figures, clean(costFigure(w))+" in "+fleetWindowPhrase(w.Window))
	}
	p.Headline = "No token spend in this snapshot has a price."
	if priced {
		p.Headline = "List-price equivalent: " + strings.Join(figures, ", ") + "."
	}
	note := "A subscription is not billed this way."
	if u.PricesAsOf != "" {
		note = "Prices as of " + clean(u.PricesAsOf) + ". " + note
	}
	p.Notes = append(p.Notes, note)
	if n := longest.Unpriced; n > 0 {
		p.Notes = append(p.Notes, si(n)+" tokens of a model with no price are not counted.")
	}

	var models []Usage
	for _, mu := range u.FleetModels {
		w, ok := windowOf(mu.Usage, longest.Window)
		if !ok {
			continue
		}
		models = append(models, w)
		p.Models = append(p.Models, pageCostRow{
			Name: clean(modelName(mu.Model)), Harness: harnessLabel(mu.Harness), Cost: clean(costFigure(w)),
			Fresh: si(w.Fresh), Cached: si(w.CacheRead), Output: si(w.Out),
		})
	}
	setShares(p.Models, models)
	p.ModelsEmpty = "No spend per model is recorded in this snapshot."

	type seat struct {
		key       usage.RosterKey
		harnesses []string
		usage     []Usage
	}
	var seats []*seat
	index := map[usage.RosterKey]*seat{}
	for _, a := range u.Accounts {
		for _, wk := range a.Workers {
			key := usage.RosterKey{Session: wk.Session, Handle: wk.Handle}
			s := index[key]
			if s == nil {
				s = &seat{key: key}
				index[key] = s
				seats = append(seats, s)
			}
			if h := harnessLabel(a.Harness); !containsString(s.harnesses, h) {
				s.harnesses = append(s.harnesses, h)
			}
			s.usage = sumUsage(s.usage, wk.Usage)
		}
	}
	// Seats are listed by cost, largest first, as the text view lists them.
	sort.SliceStable(seats, func(i, j int) bool {
		a, _ := windowOf(seats[i].usage, longest.Window)
		b, _ := windowOf(seats[j].usage, longest.Window)
		return dollarsOf(a) > dollarsOf(b)
	})
	var seatUsage []Usage
	for _, s := range seats {
		w, _ := windowOf(s.usage, longest.Window)
		row := pageCostRow{Name: clean(s.key.Handle), Meta: clean(s.key.Session) + " · " + strings.Join(s.harnesses, ", "), Cost: clean(costFigure(w))}
		if w5, ok := windowOf(s.usage, usage.Window5h); ok {
			row.Cost5h = clean(costFigure(w5))
			if !w5.LastAt.IsZero() && w5.LastContextTokens > 0 {
				row.Context = si(w5.LastContextTokens) + ", " + age(now.Sub(w5.LastAt)) + " ago"
			}
			row.CacheShare, _ = cacheReadShare(w5)
		}
		p.Seats = append(p.Seats, row)
		seatUsage = append(seatUsage, w)
	}
	for _, a := range u.Accounts {
		if a.OtherSessions == 0 {
			continue
		}
		w, _ := windowOf(a.Other, longest.Window)
		row := pageCostRow{Name: "Other sessions", Meta: otherCount(a) + " session" + plural(a.OtherSessions) + " · " + accountName(a.Key), Cost: clean(costFigure(w)), Other: true}
		if w5, ok := windowOf(a.Other, usage.Window5h); ok {
			row.Cost5h = clean(costFigure(w5))
			row.CacheShare, _ = cacheReadShare(w5)
		}
		p.Seats = append(p.Seats, row)
		seatUsage = append(seatUsage, w)
	}
	setShares(p.Seats, seatUsage)
	p.SeatsEmpty = "No spend is recorded for a seat in this snapshot."

	for _, a := range u.Accounts {
		pl := pageCostLimit{Account: accountName(a.Key)}
		if len(a.Limits) == 0 {
			pl.Lines = append(pl.Lines, pageLimitLine{Text: limitsLine(nil, now)})
		}
		for _, l := range a.Limits {
			pl.Lines = append(pl.Lines, pageLimitLine{Text: clean(limitsLine([]usage.Limit{l}, now)), Reached: l.Kind == "reached" && !l.Past})
		}
		p.Limits = append(p.Limits, pl)
	}
	return p
}

// setShares gives each priced row its part of the rows' priced total, as a
// percent with one decimal.
func setShares(rows []pageCostRow, us []Usage) {
	total := 0.0
	for _, u := range us {
		total += dollarsOf(u)
	}
	if total <= 0 {
		return
	}
	for i, u := range us {
		if u.CostUSD != nil {
			rows[i].Share = strconv.FormatFloat(100*dollarsOf(u)/total, 'f', 1, 64) + "%"
		}
	}
}

// windowOf is the usage of one window, by name.
func windowOf(us []Usage, window string) (Usage, bool) {
	for _, u := range us {
		if u.Window == window {
			return u, true
		}
	}
	return Usage{}, false
}

// fleetWindowPhrase names a trailing fleet window in a sentence.
func fleetWindowPhrase(w string) string {
	if w == usage.Window5h {
		return "the last 5 h"
	}
	return clean(windowName(w))
}

// harnessLabel names a harness as the Floor does, or by its key when the
// Floor prints no label for it.
func harnessLabel(h string) string {
	if name := harnessName(h); name != "" {
		return clean(name)
	}
	return clean(h)
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
