package cli

import (
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// wakeHoldPolicy is how long the first doorbell waits, by message priority.
// Urgent never waits. The zero value holds nothing, which is the behavior of
// a waker started without hold flags.
type wakeHoldPolicy struct {
	normal time.Duration
	low    time.Duration
}

// wakeHold is one pending message's hold: when its doorbell is due (local
// arrival plus the hold for its priority) and whether it is urgent.
type wakeHold struct {
	due    time.Time
	urgent bool
}

func (policy wakeHoldPolicy) enabled() bool {
	return policy.normal > 0 || policy.low > 0
}

func (policy wakeHoldPolicy) forPriority(priority string) time.Duration {
	switch strings.TrimSpace(priority) {
	case format.PriorityUrgent:
		return 0
	case format.PriorityLow:
		return policy.low
	default: // normal, missing or unknown
		return policy.normal
	}
}

// newWakeHold builds a message's hold from the inbox file's own modification
// time, not the sender's header clock. A restarted waker reads the same file
// and gets the same due time, so a restart does not restart the hold. A time
// in the future is clamped to now.
func newWakeHold(policy wakeHoldPolicy, priority string, info os.FileInfo, now time.Time) wakeHold {
	arrival := now
	if info != nil && info.ModTime().Before(now) {
		arrival = info.ModTime()
	}
	return wakeHold{
		due:    arrival.Add(policy.forPriority(priority)),
		urgent: strings.TrimSpace(priority) == format.PriorityUrgent,
	}
}

// earliestWakeHold is the earliest due time among pending messages that are
// not in skip; zero when there is none or holds are off.
func earliestWakeHold(
	holds map[string]wakeHold,
	current map[string]os.FileInfo,
	skip map[string]*wakeFileIdentity,
) time.Time {
	var earliest time.Time
	for name := range current {
		if _, seen := skip[name]; seen {
			continue
		}
		hold, ok := holds[name]
		if !ok {
			continue
		}
		if earliest.IsZero() || hold.due.Before(earliest) {
			earliest = hold.due
		}
	}
	return earliest
}

// wakeAdditionUrgent reports whether a message not in cohort is urgent.
func wakeAdditionUrgent(
	holds map[string]wakeHold,
	current map[string]os.FileInfo,
	cohort map[string]*wakeFileIdentity,
) bool {
	for name := range current {
		if _, seen := cohort[name]; seen {
			continue
		}
		if holds[name].urgent {
			return true
		}
	}
	return false
}
