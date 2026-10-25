package board

import (
	"fmt"
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
		other := fmt.Sprintf("other sessions in this project (%d): %s", a.OtherSessions, spendLine(a.Other, now, a.Harness == usage.Pi))
		if a.Detail != "" {
			other += " · " + a.Detail
		}
		fmt.Fprintf(b, "    %s\n", clean(other))
	}
}

// usageLine is a roster chip's spend line, or "" for a worker with no
// mapped harness session. A reached account limit is named on the chip with
// its reset time; the account's other limits never are.
func usageLine(a RosterAgent, now time.Time) string {
	var parts []string
	if len(a.Usage) > 0 {
		parts = append(parts, spendLine(a.Usage, now, false))
	}
	if l := a.LimitReached; l != nil {
		p := "limit reached"
		if l.ResetsAt != nil {
			p += " until " + l.ResetsAt.Local().Format("15:04")
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}

// spendLine prints each window's fresh and cached tokens. withCost names
// "cost not reported" for a harness that can report cost and did not.
func spendLine(us []Usage, now time.Time, withCost bool) string {
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
		case w.CostUSD != nil:
			p += fmt.Sprintf(", $%.2f %s", *w.CostUSD, w.CostKind)
		case withCost:
			p += ", cost not reported"
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
			if l.UsedPercent != nil {
				p += percent(*l.UsedPercent) + " used"
			} else {
				p += "used unknown"
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
			parts = append(parts, l.Reason)
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
