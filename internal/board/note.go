package board

import (
	"time"

	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func modelPresenceNote(a RosterAgent, now time.Time, cfg Config) string {
	note, ageSince, hasAge := presence.NoteDisplay(a.Note, a.NoteAt, now, FreshnessWindow(cfg))
	if note == "" {
		return ""
	}
	if !hasAge {
		return note + " (age unknown)"
	}
	return note + " (" + age(ageSince) + " ago)"
}
