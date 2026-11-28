package board

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
	facts "github.com/avivsinai/agent-message-queue/usage"
)

// writeUsage prints the fleet total, one limits line per account, and each
// account's spend outside mapped workers.
func writeUsage(b *strings.Builder, m Model) {
	u := m.Usage
	if u == nil {
		return
	}
	now := m.GeneratedAt
	fmt.Fprintf(b, "usage: fleet %s\n", spendLine(u.Fleet, now, false))
	if len(u.Accounts) == 0 {
		return
	}
	b.WriteString("limits:\n")
	for _, a := range u.Accounts {
		fmt.Fprintf(b, "  %s: %s\n", accountName(a.Key), clean(limitsLine(a.Limits, now)))
		other := fmt.Sprintf("other sessions in this project (%s): %s", otherCount(a), spendLine(a.Other, now, false))
		if a.Detail != "" {
			other += " · " + a.Detail
		}
		fmt.Fprintf(b, "    %s\n", clean(other))
	}
	writeCost(b, u)
}

// costTop bounds each cost line to its largest entries.
const costTop = 5

// writeCost prints the list-price block: the fleet per window, then the
// largest models and seats over the last window.
func writeCost(b *strings.Builder, u *UsageView) {
	if len(u.Fleet) == 0 {
		return
	}
	head := "cost (" + usage.ListPrice
	if u.PricesAsOf != "" {
		head += ", prices as of " + u.PricesAsOf
	}
	b.WriteString(clean(head) + "):\n")
	// The fleet's unpriced tokens are named once, for the longest window,
	// which holds the shorter one's.
	var fleet []string
	for _, w := range u.Fleet {
		w.Unpriced = 0
		fleet = append(fleet, windowName(w.Window)+" "+costFigure(w))
	}
	if n := u.Fleet[len(u.Fleet)-1].Unpriced; n > 0 {
		fleet = append(fleet, si(n)+" tokens of an unpriced model not counted")
	}
	fmt.Fprintf(b, "  fleet: %s\n", strings.Join(fleet, " · "))
	last := len(u.Fleet) - 1
	window := windowName(u.Fleet[last].Window)
	// A model id spent under more than one harness, and the unknown model,
	// name the harness, so no two entries read alike.
	harnesses := map[string]map[string]bool{}
	for _, mu := range u.FleetModels {
		if harnesses[mu.Model] == nil {
			harnesses[mu.Model] = map[string]bool{}
		}
		harnesses[mu.Model][mu.Harness] = true
	}
	var models []string
	for _, mu := range u.FleetModels {
		w := mu.Usage[last]
		name := modelName(mu.Model)
		if mu.Model == "" || len(harnesses[mu.Model]) > 1 {
			name += " (" + mu.Harness + ")"
		}
		models = append(models, name+" "+costFigure(w)+" ("+si(w.CacheRead)+" cached, "+si(w.Out)+" out)")
	}
	if len(models) > 0 {
		fmt.Fprintf(b, "  by model, %s: %s\n", window, clean(topJoin(models)))
	}
	type seat struct {
		key  usage.RosterKey
		cost Usage
	}
	var seats []seat
	index := map[usage.RosterKey]int{}
	handles := map[string]map[string]bool{}
	var other []Usage
	others := 0
	for _, a := range u.Accounts {
		for _, wk := range a.Workers {
			if last >= len(wk.Usage) {
				continue
			}
			key := usage.RosterKey{Session: wk.Session, Handle: wk.Handle}
			if handles[wk.Handle] == nil {
				handles[wk.Handle] = map[string]bool{}
			}
			handles[wk.Handle][wk.Session] = true
			i, ok := index[key]
			if !ok {
				i = len(seats)
				index[key] = i
				seats = append(seats, seat{key: key})
			}
			seats[i].cost = sumUsage([]Usage{seats[i].cost}, wk.Usage[last:last+1])[0]
		}
		if last < len(a.Other) {
			other = sumUsage(other, a.Other[last:last+1])
			others += a.OtherSessions
		}
	}
	sort.SliceStable(seats, func(i, j int) bool { return dollarsOf(seats[i].cost) > dollarsOf(seats[j].cost) })
	var parts []string
	for _, st := range seats {
		name := st.key.Handle
		if len(handles[name]) > 1 {
			name = st.key.Session + "/" + name
		}
		parts = append(parts, name+" "+costFigure(st.cost))
	}
	line := topJoin(parts)
	if len(other) > 0 && others > 0 {
		line = strings.TrimPrefix(line+" · other sessions ("+strconv.Itoa(others)+") "+costFigure(other[0]), " · ")
	}
	if line != "" {
		fmt.Fprintf(b, "  by seat, %s: %s\n", window, clean(line))
	}
}

// topJoin joins the first costTop parts and counts the rest.
func topJoin(parts []string) string {
	if len(parts) > costTop {
		parts = append(parts[:costTop:costTop], strconv.Itoa(len(parts)-costTop)+" more")
	}
	return strings.Join(parts, " · ")
}

// costFigure is a row's dollars, or "no price" when it holds tokens and no
// cost (a model with no price, or a view stored before costs were kept). A
// figure that leaves tokens out says so, and a kind other than the list
// price is named.
func costFigure(u Usage) string {
	switch {
	case u.Unknown != "":
		return "spent unknown"
	case u.CostUSD == nil && u.Unpriced+u.Fresh+u.CacheRead > 0:
		return "no price"
	case u.CostUSD == nil:
		return dollars(0)
	}
	f := dollars(*u.CostUSD)
	if u.Partial {
		f = "at least " + f
	}
	if u.CostKind != usage.ListPrice {
		f += " " + u.CostKind
	}
	if u.Unpriced > 0 {
		f += " + " + si(u.Unpriced) + " tokens unpriced"
	}
	return f
}

func dollarsOf(u Usage) float64 {
	if u.CostUSD == nil {
		return 0
	}
	return *u.CostUSD
}

func modelName(m string) string {
	if m == "" {
		return "unknown model"
	}
	return m
}

// dollars prints US dollars with two decimals and thousands separators.
func dollars(f float64) string {
	s := strconv.FormatFloat(math.Abs(f), 'f', 2, 64)
	whole, frac, _ := strings.Cut(s, ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	sign := ""
	if f < 0 {
		sign = "-"
	}
	return sign + "$" + whole + "." + frac
}

// otherCount is how many other sessions were counted: a lower bound while
// files of the account went unread, never a plain zero.
func otherCount(a AccountUsage) string {
	n := strconv.Itoa(a.OtherSessions)
	if a.Partial {
		return "at least " + n
	}
	return n
}

// usageLine is a roster chip's spend line, or "" for a worker with no
// mapped harness session. A reached account limit is named on the chip with
// its reset time, a past one with its age; the account's other limits never
// are. The context line is not part of the chip: it prints as its own
// indented line under the usage line.
func usageLine(a RosterAgent, now time.Time) string {
	var parts []string
	if len(a.Usage) > 0 {
		parts = append(parts, spendLine(a.Usage, now, true))
	}
	if l := a.LimitReached; l != nil {
		p := "limit reached"
		switch {
		case l.Past:
			p = pastLimit(*l, now)
		case l.ResetsAt != nil:
			p += " until " + l.ResetsAt.Local().Format("15:04")
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}

// contextLine is the seat's context line: the latest response's prompt size
// with its age, and the 5 h cache-read share when the account records cache
// reads. A worker with no counted response shows neither.
func contextLine(a RosterAgent, now time.Time) string {
	if len(a.Usage) == 0 || a.Usage[0].LastAt.IsZero() {
		return ""
	}
	last := a.Usage[0]
	// A response with no prompt tokens (a pure output turn) says nothing
	// about context size, so the line stays off.
	if last.LastContextTokens == 0 {
		return ""
	}
	line := fmt.Sprintf("context %s at %s", si(last.LastContextTokens), age(now.Sub(last.LastAt))+" ago")
	if share, ok := cacheReadShare(last); ok {
		line += " · cache reads " + share + " of 5 h"
	}
	return line
}

// cacheReadShare is a window's cache reads as a percent of its fresh and
// cached tokens. The share needs cache reads: a harness that records none
// (pi) or a window with none has no percentage. Truncated to one decimal,
// never rounded: a near-total share stays below 100 and only a genuinely
// zero fresh count prints 100.0.
func cacheReadShare(u Usage) (string, bool) {
	if u.CacheRead <= 0 {
		return "", false
	}
	share := math.Floor(1000*float64(u.CacheRead)/float64(u.Fresh+u.CacheRead)) / 10
	return fmt.Sprintf("%.1f%%", share), true
}

// pastLimit names a reached limit the account spent tokens after.
func pastLimit(l usage.Limit, now time.Time) string {
	p := "limit reported"
	if l.Name != "" {
		p = limitName(l.Name) + " " + p
	}
	if !l.AsOf.IsZero() {
		p += " " + age(now.Sub(l.AsOf)) + " ago"
	}
	return p + "; the account was active since then"
}

// spendLine prints each window's fresh and cached tokens. A seat's line
// also prints its 5 h cost; every other figure of money is in the cost
// block.
func spendLine(us []Usage, now time.Time, seat bool) string {
	var parts []string
	var asOf time.Time
	for _, w := range us {
		if w.Unknown != "" {
			parts = append(parts, windowName(w.Window)+" spent unknown ("+clean(w.Unknown)+")")
			continue
		}
		p := windowName(w.Window) + " "
		if w.Partial {
			p += "at least "
		}
		p += si(w.Fresh) + " fresh, " + si(w.CacheRead) + " cached"
		switch {
		case !seat || w.Window != usage.Window5h:
		case w.CostUSD != nil:
			p += ", " + dollars(*w.CostUSD) + " " + w.CostKind
			if w.Unpriced > 0 {
				p += " + " + si(w.Unpriced) + " tokens unpriced"
			}
		case w.Unpriced > 0:
			p += ", no price"
		}
		parts = append(parts, p)
		asOf = later(asOf, w.AsOf)
	}
	if !asOf.IsZero() {
		parts = append(parts, "newest "+age(now.Sub(asOf))+" ago")
	}
	return strings.Join(parts, " · ")
}

// limitsLine prints an account's limits. Unknown is a word with its reason,
// never a blank.
// App-server readings are dated once, at the end.
func limitsLine(ls []usage.Limit, now time.Time) string {
	var parts []string
	var read time.Time
	stale := false
	for _, l := range ls {
		bucket := ""
		if l.Bucket != "" {
			bucket = l.Bucket + " "
		}
		if l.Source == appServer {
			read = later(read, l.AsOf)
			stale = stale || l.Status == string(facts.StatusStale)
		}
		switch {
		case l.Kind == "window":
			p := bucket + windowLimitName(l) + " "
			switch {
			case l.UsedPercent == nil:
				p += "used unknown"
			case paceAhead(l, now) > 0:
				p += percent(*l.UsedPercent) + " used at " + strconv.Itoa(paceAhead(l, now)) + "% of the window: on pace to reach the limit before it resets"
			default:
				p += percent(*l.UsedPercent) + " used"
			}
			if l.ResetsAt != nil {
				p += ", resets " + resetTime(*l.ResetsAt, now)
			}
			parts = append(parts, p)
		case l.Kind == "cap":
			p := bucket + "spend cap " + amount(l.Used) + " of " + amount(l.Cap) + " used"
			if l.RemainingPercent != nil {
				p += " (" + percent(*l.RemainingPercent) + " left)"
			}
			if l.ResetsAt != nil {
				p += ", resets " + resetTime(*l.ResetsAt, now)
			}
			parts = append(parts, p)
		case l.Kind == "reached" && l.Past:
			parts = append(parts, pastLimit(l, now))
		case l.Kind == "reached" && l.Source == appServer:
			parts = append(parts, bucket+strings.ReplaceAll(l.Name, "_", " "))
		case l.Kind == "reached":
			p := limitName(l.Name) + " limit reached"
			if l.ResetsAt != nil {
				p += ", resets " + l.ResetsAt.Local().Format("15:04")
			}
			parts = append(parts, p)
		case l.Kind == "credits" && l.Source == appServer && l.Unlimited:
			parts = append(parts, bucket+"unlimited credits")
		case l.Kind == "credits" && l.Source == appServer:
			parts = append(parts, bucket+credits(l.Balance)+" credits")
		case l.Kind == "credits":
			parts = append(parts, credits(l.Balance)+" credits as of "+age(now.Sub(l.AsOf))+" ago")
		default:
			parts = append(parts, bucket+l.Reason)
		}
	}
	if len(parts) == 0 {
		return "limit unknown"
	}
	if !read.IsZero() {
		p := "read " + age(now.Sub(read)) + " ago"
		if stale {
			p += " (stale)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}

// paceAhead is the elapsed percent of a plan window when the reading is
// at least half used and ahead of the window's clock, so the limit is on
// pace to be reached before the reset; otherwise 0. The window's clock is
// read at the reading's time, when the used percent was true.
func paceAhead(l usage.Limit, now time.Time) int {
	if l.UsedPercent == nil || l.WindowMinutes <= 0 || l.ResetsAt == nil || !l.ResetsAt.After(now) {
		return 0
	}
	at := l.AsOf
	if at.IsZero() {
		at = now
	}
	window := time.Duration(l.WindowMinutes) * time.Minute
	elapsed := 100 * float64(window-l.ResetsAt.Sub(at)) / float64(window)
	used := *l.UsedPercent
	if elapsed <= 0 || elapsed >= 100 || used < 50 || used >= 100 || used <= elapsed {
		return 0
	}
	return max(1, int(math.Round(elapsed)))
}

// windowLimitName names a plan window by its duration.
func windowLimitName(l usage.Limit) string {
	switch {
	case l.Name == facts.Window5h:
		return "5 h"
	case l.Name == facts.WindowWeekly:
		return "weekly"
	case l.WindowMinutes > 0 && l.WindowMinutes%60 == 0:
		return strconv.FormatInt(l.WindowMinutes/60, 10) + " h window"
	case l.WindowMinutes > 0:
		return strconv.FormatInt(l.WindowMinutes, 10) + " min window"
	}
	return "window"
}

// resetTime prints a reset inside the next day as a clock time, a later one
// with its date.
func resetTime(t, now time.Time) string {
	if d := t.Sub(now); d >= 0 && d < 24*time.Hour {
		return t.Local().Format("15:04")
	}
	return t.Local().Format("Jan 2 15:04")
}

func percent(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) + "%" }

// amount prints a decimal string with two decimals; the JSON face keeps the
// source's string verbatim.
func amount(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return clean(s)
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

func accountName(key string) string {
	harness, home, _ := strings.Cut(key, ":")
	switch harness {
	case usage.Claude:
		harness = "Claude"
	case usage.Codex:
		harness = "Codex"
	case usage.Pi:
		harness = "pi"
	}
	return clean(harness + " " + home)
}

func windowName(w string) string {
	switch w {
	case usage.Window5h:
		return "5 h"
	case usage.Window7d:
		return "7 d"
	}
	return w
}

func limitName(n string) string {
	switch n {
	case "five_hour":
		return "5 h"
	case "seven_day":
		return "7 d"
	}
	return clean(n)
}

// credits prints a decimal balance with one decimal; the JSON face keeps
// the harness's string verbatim.
func credits(balance string) string {
	f, err := strconv.ParseFloat(balance, 64)
	if err != nil {
		return clean(balance)
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}

// si prints a token count with a K, M or G suffix.
func si(n int64) string {
	switch {
	case n >= 1e9:
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + "G"
	case n >= 1e6:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1e3:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "K"
	}
	return strconv.FormatInt(n, 10)
}
