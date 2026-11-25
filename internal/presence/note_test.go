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
		name, at string
		window   time.Duration
		wantAge  time.Duration
		known    bool
	}{
		{name: "raw old note", at: now.Add(-40 * time.Hour).Format(time.RFC3339), wantAge: 40 * time.Hour, known: true},
		{name: "Floor old note", at: now.Add(-40 * time.Hour).Format(time.RFC3339), window: 30 * time.Minute, known: false},
		{name: "Floor at bound", at: now.Add(-30 * time.Minute).Format(time.RFC3339), window: 30 * time.Minute, known: false},
		{name: "legacy", at: "", known: false},
		{name: "invalid", at: "not-a-time", known: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, age, known := NoteDisplay("waiting", row.at, now, row.window)
			if row.name == "Floor at bound" {
				if got != "waiting" || !known {
					t.Fatalf("the bound note was hidden: %q %v", got, known)
				}
				return
			}
			if row.window > 0 {
				if got != "" {
					t.Fatalf("Floor retained %q", got)
				}
				return
			}
			if got != "waiting" || known != row.known || age != row.wantAge {
				t.Fatalf("note=%q age=%v known=%v", got, age, known)
			}
		})
	}
}
