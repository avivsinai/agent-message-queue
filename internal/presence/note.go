package presence

import "time"

// NoteDisplay returns the published note and its own age. Unknown is
// explicit for legacy records. A wake's newer LastSeen never dates the note.
func NoteDisplay(note, noteAt string, now time.Time, window time.Duration) (text string, age time.Duration, known bool) {
	if note == "" {
		return "", 0, true
	}
	if noteAt == "" {
		return note, 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, noteAt)
	if err != nil {
		return note, 0, false
	}
	since := now.Sub(t)
	if since >= window {
		return "", 0, false
	}
	return note, max(0, since), true
}
