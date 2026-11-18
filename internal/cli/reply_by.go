package cli

import (
	"fmt"
	"time"
)

// replyByRelative translates a reply deadline to the sender's description.
// It computes at print time and requires valid RFC 3339 for a relative hint.
func replyByRelative(replyBy string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, replyBy)
	if err != nil {
		return ""
	}
	delta := t.Sub(now)
	future := delta > 0
	if !future {
		delta = -delta
	}
	var unit string
	switch {
	case delta < time.Minute:
		unit = "now"
	case delta < time.Hour:
		unit = fmt.Sprintf("%dm", int(delta/time.Minute))
	case delta < 48*time.Hour:
		unit = fmt.Sprintf("%dh", int(delta/time.Hour))
	default:
		unit = fmt.Sprintf("%dd", int(delta/(24*time.Hour)))
	}
	if future {
		return "in " + unit
	}
	return unit + " ago"
}
