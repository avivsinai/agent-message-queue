package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

type dialogTool struct {
	Tool    string  `json:"tool"`
	Dialogs int     `json:"dialogs"`
	Held    float64 `json:"held_seconds"`
}

type dialogSession struct {
	Binding string         `json:"binding"`
	Dialogs int            `json:"dialogs"`
	Held    float64        `json:"held_seconds"`
	Median  float64        `json:"held_median_seconds"`
	Max     float64        `json:"held_max_seconds"`
	By      map[string]int `json:"by"`
	Tools   []dialogTool   `json:"tools"`
}

type dialogReport struct {
	Schema   string          `json:"schema"`
	Since    time.Time       `json:"since"`
	Sessions []dialogSession `json:"sessions"`
}

func dialogs(args []string, stdout io.Writer, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("dialogs", flag.ContinueOnError)
	c := &common{}
	addJSONFlag(fs, c, probe)
	days := fs.Int("days", 14, "number of days of dialog history")
	if err := fs.Parse(args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if fs.NArg() != 0 || *days < 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "dialogs takes no positional arguments and --days must be at least 1")
	}
	now := time.Now().UTC()
	var since time.Time
	// A very large window includes every timestamp the JSON report can represent.
	if int64(*days) <= (now.Unix()-since.Unix())/(24*60*60) {
		since = now.AddDate(0, 0, -*days)
	}
	entries, err := dialog.History(since)
	if err != nil {
		return nil, protocol.ExitActionRequired, err
	}
	report := dialogReport{Schema: "amq/remote-dialogs/v1", Since: since, Sessions: summarizeDialogs(entries)}
	if c.json {
		return report, 0, nil
	}
	return nil, 0, report.writeText(stdout, *days)
}

func summarizeDialogs(entries []dialog.HistoryEntry) []dialogSession {
	groups := map[string][]dialog.HistoryEntry{}
	for _, entry := range entries {
		groups[entry.Binding] = append(groups[entry.Binding], entry)
	}
	rows := make([]dialogSession, 0, len(groups))
	for name, group := range groups {
		row := dialogSession{Binding: name, Dialogs: len(group), By: map[string]int{}}
		var waits []float64
		tools := map[string]dialogTool{}
		for _, e := range group {
			if e.OpenedAt.IsZero() {
				continue
			}
			seconds := max(0, e.At.Sub(e.OpenedAt).Seconds())
			waits = append(waits, seconds)
			row.Held += seconds
			row.By[e.By]++
			tool := tools[e.Tool]
			tool.Tool = e.Tool
			tool.Dialogs++
			tool.Held += seconds
			tools[e.Tool] = tool
		}
		if n := len(waits); n > 0 {
			slices.Sort(waits)
			row.Median = (waits[(n-1)/2] + waits[n/2]) / 2
			row.Max = waits[n-1]
		}
		row.Tools = make([]dialogTool, 0, len(tools))
		for _, tool := range tools {
			row.Tools = append(row.Tools, tool)
		}
		slices.SortFunc(row.Tools, func(a, b dialogTool) int {
			return cmp.Or(cmp.Compare(b.Held, a.Held), cmp.Compare(a.Tool, b.Tool))
		})
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b dialogSession) int {
		return cmp.Or(cmp.Compare(b.Held, a.Held), cmp.Compare(a.Binding, b.Binding))
	})
	return rows
}

func (r dialogReport) writeText(w io.Writer, days int) error {
	var dialogs int
	var seconds float64
	for _, s := range r.Sessions {
		dialogs += s.Dialogs
		seconds += s.Held
	}
	say(w, "Approval dialogs, last %d days · %d dialogs · held %s", days, dialogs, heldTime(seconds))
	if dialogs == 0 {
		say(w, "No dialogs in this period.")
		return nil
	}
	table := tabwriter.NewWriter(w, 0, 1, 2, ' ', 0)
	say(table, "session\tdialogs\theld (total / median / max)\tanswered in\ttop tools by time held")
	for _, s := range r.Sessions {
		var answers, tools []string
		for _, by := range slices.Sorted(maps.Keys(s.By)) {
			label := by
			if by == dialog.ByTimeout {
				label = "expired"
			}
			answers = append(answers, fmt.Sprintf("%s %d", label, s.By[by]))
		}
		for _, tool := range s.Tools[:min(3, len(s.Tools))] {
			tools = append(tools, cmp.Or(tool.Tool, "(question)")+" "+heldTime(tool.Held))
		}
		say(table, "%s\t%d\t%s / %s / %s\t%s\t%s", s.Binding, s.Dialogs, heldTime(s.Held), heldTime(s.Median),
			heldTime(s.Max), strings.Join(answers, " · "), strings.Join(tools, " · "))
	}
	return table.Flush()
}

func heldTime(seconds float64) string {
	if seconds == 0 {
		return "0m"
	}
	if seconds < 60 {
		return "<1m"
	}
	minutes := int64(seconds/60 + 0.5)
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
}
