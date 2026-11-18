package board

import (
	"cmp"
	"slices"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// Ask is one open obligation of the snapshot: who owes a reply to whom,
// and by when if the request names a time.
type Ask struct {
	Key     string      `json:"key"`
	Kind    string      `json:"kind"`
	Ruling  bool        `json:"ruling,omitempty"`
	Subject string      `json:"subject"`
	From    Participant `json:"from"`
	Owner   Participant `json:"owner"`
	Human   bool        `json:"human,omitempty"`
	Opened  time.Time   `json:"opened,omitzero"`
	ReplyBy time.Time   `json:"reply_by,omitzero"`
}

func (cfg Config) obligationCutoff(now time.Time) time.Time {
	maxAge := thread.DefaultObligationMaxAge
	if cfg.ObligationMaxAge != "" {
		if age, err := ParseDuration(cfg.ObligationMaxAge); err == nil {
			maxAge = age
		}
	}
	if maxAge == 0 {
		return time.Time{}
	}
	return now.Add(-maxAge)
}

// projectAsks records whether any recipient still owes on each opener.
// Only human obligations and timed agent obligations go on the board.
func (s Snapshot) projectAsks(cutoff time.Time) (map[string]bool, []Ask) {
	open := map[string]bool{}
	asks := []Ask{}
	for key, ob := range s.openObligations(cutoff) {
		open[key] = true
		m := s.Messages[key]
		human := ob.Owner.Handle == reservedHuman
		due := replyTime(m)
		if !human && due.IsZero() {
			continue
		}
		asks = append(asks, Ask{
			Key: key, Kind: m.Kind, Ruling: slices.Contains(m.Labels, "ruling"), Subject: m.Subject,
			From: Participant(ob.Requester), Owner: Participant(ob.Owner), Human: human,
			Opened: ob.Opener.RawTime.UTC(), ReplyBy: due,
		})
	}
	slices.SortFunc(asks, func(a, b Ask) int {
		if a.Human != b.Human {
			if a.Human {
				return -1
			}
			return 1
		}
		if a.ReplyBy.IsZero() != b.ReplyBy.IsZero() {
			if a.ReplyBy.IsZero() {
				return 1
			}
			return -1
		}
		return cmp.Or(
			a.ReplyBy.Compare(b.ReplyBy),
			b.Opened.Compare(a.Opened),
			cmp.Compare(a.Key, b.Key),
			cmp.Compare(a.Owner.Project, b.Owner.Project),
			cmp.Compare(a.Owner.Session, b.Owner.Session),
			cmp.Compare(a.Owner.Handle, b.Owner.Handle),
		)
	})
	return open, asks
}

func replyTime(m Message) time.Time {
	at, _ := time.Parse(time.RFC3339, m.ReplyBy)
	return at.UTC()
}
