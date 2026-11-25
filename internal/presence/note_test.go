package presence

import (
	"testing"
	"time"
)

// Raw presence listing hid a forty-hour-old note instead of reporting its age.
// Only the board Floor applies a freshness cutoff to note text.
func TestNoteDisplayRawListingKeepsOldAndUndatedNotes(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name    string
		at      string
		window  time.Duration
		want    string
		wantAge time.Duration
		known   bool
	}{
		{name: "raw old note", at: now.Add(-40 * time.Hour).Format(time.RFC3339), want: "waiting", wantAge: 40 * time.Hour, known: true},
		{name: "Floor old note", at: now.Add(-40 * time.Hour).Format(time.RFC3339), window: 30 * time.Minute, want: "", known: false},
		{name: "Floor at bound", at: now.Add(-30 * time.Minute).Format(time.RFC3339), window: 30 * time.Minute, want: "waiting", wantAge: 30 * time.Minute, known: true},
		{name: "legacy", at: "", want: "waiting", known: false},
		{name: "invalid", at: "not-a-time", want: "waiting", known: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, age, known := NoteDisplay("waiting", row.at, now, row.window)
			if got != row.want || known != row.known || age != row.wantAge {
				t.Fatalf("note=%q age=%v known=%v, want note=%q age=%v known=%v", got, age, known, row.want, row.wantAge, row.known)
			}
		})
	}
}
