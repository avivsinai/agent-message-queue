package board

import (
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// Usage is what was spent in one window: per worker, per account's other
// sessions, or the whole fleet. Fresh is uncached input plus cache writes
// plus output; cache reads are shown beside it, never summed into it.
type Usage struct {
	Window     string    `json:"window"`
	WindowFrom time.Time `json:"window_from"`
	Label      string    `json:"label"`
	In         int64     `json:"input"`
	CacheRead  int64     `json:"cache_read"`
	CacheWrite int64     `json:"cache_write"`
	Out        int64     `json:"output"`
	Fresh      int64     `json:"fresh"`
	CostUSD    *float64  `json:"cost_usd,omitempty"`
	CostKind   string    `json:"cost_kind,omitempty"`
	AsOf       time.Time `json:"as_of,omitzero"`
	// Partial means some session files were not read: the figures are
	// "at least".
	Partial bool `json:"partial"`
}

// UsageView is the usage part of the board: the fleet total and, per
// harness account, its limits, its mapped workers and its other sessions in
// this project.
type UsageView struct {
	Fleet    []Usage        `json:"fleet"`
	Accounts []AccountUsage `json:"accounts"`
}

// AccountUsage is one harness account. Limits are account facts and never
// appear on a worker.
type AccountUsage struct {
	Key     string        `json:"key"`
	Harness string        `json:"harness"`
	Limits  []usage.Limit `json:"limits"`
	Workers []WorkerUsage `json:"workers"`
	// Other is the spend of eligible sessions no structural record maps to
	// a worker.
	Other         []Usage `json:"other"`
	OtherSessions int     `json:"other_sessions"`
	Partial       bool    `json:"partial"`
	Detail        string  `json:"detail,omitempty"`
}

// WorkerUsage is one worker's spend on one account and the harness session
// ids it was counted from.
type WorkerUsage struct {
	Session  string   `json:"session"`
	Handle   string   `json:"handle"`
	Usage    []Usage  `json:"usage"`
	Sessions []string `json:"harness_sessions"`
}

// UsageMapping maps each harness session an AMQ record ties to a worker.
// Spend is history, so ready and revoked records attribute it; uncertain
// ones do not. A harness session two records tie to different workers
// attributes to neither.
func UsageMapping(convs []Conversation) usage.Mapping {
	out := usage.Mapping{}
	conflict := map[usage.SessionRef]bool{}
	for _, c := range convs {
		if c.State != "ready" && c.State != "revoked" {
			continue
		}
		ref := usage.SessionRef{Harness: c.Provider, ID: c.ID}
		key := usage.RosterKey{Session: c.Session, Handle: c.Handle}
		if prev, ok := out[ref]; ok && prev != key {
			conflict[ref] = true
		}
		out[ref] = key
	}
	for ref := range conflict {
		delete(out, ref)
	}
	return out
}

// WithUsage adds the usage report to the model: per account limits, per
// worker spend through mapping, the rest as the account's other sessions,
// and the fleet total. It is pure; r.Now dates the windows.
func WithUsage(m Model, r usage.Report, mapping usage.Mapping) Model {
	view := &UsageView{Accounts: []AccountUsage{}}
	perWorker := map[usage.RosterKey][]Usage{}
	reached := map[usage.RosterKey]*usage.Limit{}
	var fleet []Usage
	for _, a := range r.Accounts {
		windows := accountWindows(a, r.Now)
		au := AccountUsage{Key: a.Key, Harness: a.Harness, Limits: a.Limits, Workers: []WorkerUsage{}, Partial: a.Partial(), Detail: a.Detail}
		if au.Limits == nil {
			au.Limits = []usage.Limit{}
		}
		workers := map[usage.RosterKey]*WorkerUsage{}
		other := make([]usage.Counts, len(windows))
		otherAsOf := make([]time.Time, len(windows))
		workerCounts := map[usage.RosterKey][]usage.Counts{}
		workerAsOf := map[usage.RosterKey][]time.Time{}
		for _, s := range a.Sessions {
			key, mapped := mapping[usage.SessionRef{Harness: s.Harness, ID: s.ID}]
			if mapped {
				w := workers[key]
				if w == nil {
					w = &WorkerUsage{Session: key.Session, Handle: key.Handle}
					workers[key] = w
					workerCounts[key] = make([]usage.Counts, len(windows))
					workerAsOf[key] = make([]time.Time, len(windows))
				}
				w.Sessions = append(w.Sessions, s.ID)
			} else {
				au.OtherSessions++
			}
			for i, win := range windows {
				c, asOf := s.Totals(win.from)
				if mapped {
					workerCounts[key][i] = usage.Add(workerCounts[key][i], c)
					workerAsOf[key][i] = later(workerAsOf[key][i], asOf)
				} else {
					other[i] = usage.Add(other[i], c)
					otherAsOf[i] = later(otherAsOf[i], asOf)
				}
			}
		}
		for key, w := range workers {
			for i, win := range windows {
				w.Usage = append(w.Usage, makeUsage(win, workerCounts[key][i], workerAsOf[key][i], au.Partial))
			}
			au.Workers = append(au.Workers, *w)
			perWorker[key] = sumUsage(perWorker[key], w.Usage)
			if l := reachedLimit(a.Limits); l != nil {
				reached[key] = l
			}
		}
		sort.Slice(au.Workers, func(i, j int) bool {
			if au.Workers[i].Session != au.Workers[j].Session {
				return au.Workers[i].Session < au.Workers[j].Session
			}
			return au.Workers[i].Handle < au.Workers[j].Handle
		})
		for i, win := range windows {
			au.Other = append(au.Other, makeUsage(win, other[i], otherAsOf[i], au.Partial))
		}
		fleet = sumUsage(fleet, au.Other)
		for _, w := range au.Workers {
			fleet = sumUsage(fleet, w.Usage)
		}
		view.Accounts = append(view.Accounts, au)
	}
	view.Fleet = fleet
	m.Usage = view
	for i := range m.Roster {
		for j := range m.Roster[i].Agents {
			a := &m.Roster[i].Agents[j]
			key := usage.RosterKey{Session: a.Session, Handle: a.Handle}
			a.Usage, a.LimitReached = perWorker[key], reached[key]
		}
		for j := range m.Roster[i].Quiet {
			a := &m.Roster[i].Quiet[j]
			key := usage.RosterKey{Session: a.Session, Handle: a.Handle}
			a.Usage, a.LimitReached = perWorker[key], reached[key]
		}
	}
	return m
}

// reachedLimit is the account's reached limit, if it reports one.
func reachedLimit(ls []usage.Limit) *usage.Limit {
	for i := range ls {
		if ls[i].Kind == "reached" {
			l := ls[i]
			return &l
		}
	}
	return nil
}

type window struct {
	name, label string
	from        time.Time
}

// accountWindows are the account's 5 h and 7 d windows: aligned to a
// reported reset when the account has one ahead, else trailing.
func accountWindows(a usage.Account, now time.Time) []window {
	ws := []window{
		{name: usage.Window5h, label: "last 5 h", from: now.Add(-5 * time.Hour)},
		{name: usage.Window7d, label: "last 7 d", from: now.Add(-7 * 24 * time.Hour)},
	}
	for _, l := range a.Limits {
		if l.ResetsAt == nil || !l.ResetsAt.After(now) {
			continue
		}
		switch l.Name {
		case "five_hour":
			ws[0].from, ws[0].label = l.ResetsAt.Add(-5*time.Hour), "5 h window"
		case "seven_day":
			ws[1].from, ws[1].label = l.ResetsAt.Add(-7*24*time.Hour), "7 d window"
		}
	}
	return ws
}

func makeUsage(w window, c usage.Counts, asOf time.Time, partial bool) Usage {
	return Usage{
		Window: w.name, WindowFrom: w.from.UTC(), Label: w.label,
		In: c.In, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite, Out: c.Out, Fresh: c.Fresh(),
		CostUSD: c.Cost, CostKind: c.CostKind, AsOf: asOf, Partial: partial,
	}
}

// sumUsage adds b into a window by window. The first operand's labels win;
// a sum of differently aligned windows keeps the window name only.
func sumUsage(a, b []Usage) []Usage {
	if a == nil {
		return append([]Usage(nil), b...)
	}
	for i := range a {
		if i >= len(b) {
			break
		}
		x, y := a[i], b[i]
		x.In += y.In
		x.CacheRead += y.CacheRead
		x.CacheWrite += y.CacheWrite
		x.Out += y.Out
		x.Fresh += y.Fresh
		if y.CostUSD != nil {
			sum := *y.CostUSD
			if x.CostUSD != nil {
				sum += *x.CostUSD
			}
			x.CostUSD = &sum
			if x.CostKind != y.CostKind && x.CostKind != "" {
				x.CostKind = "mixed estimates and billed"
			} else {
				x.CostKind = y.CostKind
			}
		}
		x.AsOf = later(x.AsOf, y.AsOf)
		x.Partial = x.Partial || y.Partial
		if !x.WindowFrom.Equal(y.WindowFrom) {
			x.Label = "last " + map[string]string{usage.Window5h: "5 h", usage.Window7d: "7 d"}[x.Window]
			x.WindowFrom = earlier(x.WindowFrom, y.WindowFrom)
		}
		a[i] = x
	}
	return a
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
