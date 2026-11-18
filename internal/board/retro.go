package board

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// RetroSchema is the JSON schema name of a retro document.
const RetroSchema = "amq/board-retro/v1"

// RetroSeat is the request history of one owner of requests in the period.
type RetroSeat struct {
	Session          string  `json:"session,omitempty"`
	Handle           string  `json:"handle"`
	Human            bool    `json:"human,omitempty"`
	Requests         int     `json:"requests"`
	Open             int     `json:"open"`
	Replied          int     `json:"replied"`
	Closed           int     `json:"closed"`
	FirstReplyMedian float64 `json:"first_reply_median_seconds,omitempty"`
	FirstReplyMax    float64 `json:"first_reply_max_seconds,omitempty"`
	DoneMedian       float64 `json:"done_median_seconds,omitempty"`
	DoneMax          float64 `json:"done_max_seconds,omitempty"`
	FollowUps        int     `json:"follow_ups"`
	Rounds           int     `json:"rework_rounds"`
	RoundsMax        int     `json:"rework_rounds_max"`
}

// RetroView is the measured request history since a time.
type RetroView struct {
	Since    time.Time   `json:"since"`
	Requests int         `json:"requests"`
	Seats    []RetroSeat `json:"seats"`
}

type retroSamples struct {
	RetroSeat
	firstReplies []float64
	completions  []float64
}

// Retro measures the requests opened at or after since. It is pure.
func (s Snapshot) Retro(since time.Time) RetroView {
	view := RetroView{Since: since, Seats: []RetroSeat{}}
	type seatKey struct{ session, handle string }
	seats := map[seatKey]*retroSamples{}
	for _, ns := range s.requestNamespaces() {
		// A reply carries the entire lineage, including its direct parent.
		conversations := make(map[string][]*thread.Entry)
		for i := range ns.entries {
			msg := &ns.entries[i]
			for _, ref := range msg.Refs {
				messages := conversations[ref]
				if len(messages) == 0 || messages[len(messages)-1] != msg {
					conversations[ref] = append(messages, msg)
				}
			}
		}
		for _, ob := range ns.obligations(s) {
			if ob.Opener.RawTime.IsZero() || ob.Opener.RawTime.Before(since) {
				continue
			}
			key := seatKey{ob.Owner.Session, ob.Owner.Handle}
			seat := seats[key]
			if seat == nil {
				seat = &retroSamples{RetroSeat: RetroSeat{Session: key.session, Handle: key.handle, Human: key.handle == thread.HumanHandle}}
				seats[key] = seat
			}
			view.Requests++
			seat.record(ob, conversations[ob.Opener.ID])
		}
	}
	for _, seat := range seats {
		seat.FirstReplyMedian, seat.FirstReplyMax = medianMax(seat.firstReplies)
		seat.DoneMedian, seat.DoneMax = medianMax(seat.completions)
		view.Seats = append(view.Seats, seat.RetroSeat)
	}
	slices.SortFunc(view.Seats, func(a, b RetroSeat) int {
		if a.Human != b.Human {
			if a.Human {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Session, b.Session), cmp.Compare(a.Handle, b.Handle))
	})
	return view
}

func (seat *retroSamples) record(ob *thread.Obligation, messages []*thread.Entry) {
	start := ob.Opener.RawTime
	seat.Requests++
	if ob.Open() {
		seat.Open++
	}
	if ob.Closed && ob.ClosedBy != nil {
		seat.Closed++
		seat.completions = append(seat.completions, ob.ClosedBy.RawTime.Sub(start).Seconds())
	}

	var first time.Time
	rounds := 0
	for _, m := range messages {
		if m.From == ob.Owner.Handle && !m.RawTime.IsZero() && (first.IsZero() || m.RawTime.Before(first)) {
			first = m.RawTime
		}
		if slices.Contains(m.Labels, "follow-up") {
			if m.Refs[0] == ob.Opener.ID {
				seat.FollowUps++
			}
			continue
		}
		if m.From == ob.Requester.Handle && (ob.ClosedBy == nil || m.ID != ob.ClosedBy.ID) {
			rounds++
		}
	}
	if !first.IsZero() {
		seat.Replied++
		seat.firstReplies = append(seat.firstReplies, first.Sub(start).Seconds())
	}
	seat.Rounds += rounds
	seat.RoundsMax = max(seat.RoundsMax, rounds)
}

func medianMax(samples []float64) (float64, float64) {
	n := len(samples)
	if n == 0 {
		return 0, 0
	}
	slices.Sort(samples)
	return (samples[(n-1)/2] + samples[n/2]) / 2, samples[n-1]
}

// WriteRetroText renders the seat measurements and any gap in history coverage.
func (s Snapshot) WriteRetroText(w io.Writer, view RetroView, period string) error {
	if _, err := fmt.Fprintf(w, "Retro since %s (%s) · %d requests\n", view.Since.Format(time.RFC3339), clean(period), view.Requests); err != nil {
		return err
	}
	if view.Requests == 0 {
		_, err := fmt.Fprintln(w, "No requests in this period.")
		return err
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "seat\trequests\topen\tfirst reply (median / max)\tdone (median / max)\tfollow-ups\trework rounds (total / max)"); err != nil {
		return err
	}
	for _, seat := range view.Seats {
		name := clean(seat.Session + "/" + seat.Handle)
		if seat.Human {
			name = "you"
		}
		_, err := fmt.Fprintf(table, "%s\t%d\t%d\t%s\t%s\t%d\t%d / %d\n", name, seat.Requests, seat.Open,
			durationPair(seat.Replied, seat.FirstReplyMedian, seat.FirstReplyMax),
			durationPair(seat.Closed, seat.DoneMedian, seat.DoneMax), seat.FollowUps, seat.Rounds, seat.RoundsMax)
		if err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if !historyComplete(s) {
		_, err := fmt.Fprintf(w, "history: %s\n", s.historyGapText())
		return err
	}
	return nil
}

func durationPair(count int, median, longest float64) string {
	if count == 0 {
		return "-"
	}
	return age(seconds(median)) + " / " + age(seconds(longest))
}

func (s Snapshot) historyGapText() string {
	for _, nr := range s.Input.Results {
		if nr.Name == "amq-history" {
			r := nr.Result
			src := SourceView{Name: nr.Name, Status: r.Status, Detail: r.Detail, Coverage: r.Coverage, Partial: r.Coverage.Partial()}
			status := string(src.Status)
			if src.Partial {
				status += " (partial)"
			}
			return status + ": " + sourceGap(src).Gap
		}
	}
	return "unavailable: no history source in the snapshot"
}
