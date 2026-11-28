package board

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A decision record (a message of kind decision whose context.decision
// carries decided) is listed under "Decided without you" inside the decided
// window, newest first, with one overrule command per option not taken. It
// never counts as waiting: Needs you does not count it, and a one-way door
// or a message outside the window is not a record row.
func TestInboxListsDecidedRecordsForOverrule(t *testing.T) {
	record := requestMessage("rec", "decision", "p2p/lead__user", "lead", []string{reservedHuman}, "s1", requestNow.Add(-2*time.Hour))
	if err := json.Unmarshal([]byte(`{"decision":{"options":[{"label":"Three retries"},{"label":"One retry"}],"recommended":"Three retries","blocking":false,"decided":"Three retries","if_later":"change the setting back"}}`), &record.Context); err != nil {
		t.Fatal(err)
	}
	outside := requestMessage("old", "decision", "p2p/lead__user", "lead", []string{reservedHuman}, "s1", requestNow.Add(-25*time.Hour))
	if err := json.Unmarshal([]byte(`{"decision":{"options":[{"label":"A"},{"label":"B"}],"decided":"A"}}`), &outside.Context); err != nil {
		t.Fatal(err)
	}
	s := requestSnapshot(record, outside)
	m := s.Project(DefaultConfig(), requestNow, "test")
	if len(m.Inbox.Decided) != 1 {
		t.Fatalf("decided records = %d, want 1", len(m.Inbox.Decided))
	}
	rec := m.Inbox.Decided[0]
	if rec.Decided != "Three retries" || len(rec.NotTaken) != 1 || rec.NotTaken[0] != "One retry" {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.Overrule) != 1 || rec.Overrule[0].Command == "" {
		t.Fatalf("overrule = %+v, want one command", rec.Overrule)
	}
	want := "amq reply --id rec --me user --kind todo --body " + Quote([]string{"Overrule: choose One retry"})
	if rec.Overrule[0].Command != want {
		t.Errorf("overrule = %q, want %q", rec.Overrule[0].Command, want)
	}
	if len(m.Inbox.Items) != 0 {
		t.Errorf("a record is not a waiting item, got %d", len(m.Inbox.Items))
	}
	var text strings.Builder
	if err := WriteText(&text, m); err != nil {
		t.Fatal(err)
	}
	out := text.String()
	for _, want := range []string{"nothing waits on you", "Decided without you", `lead chose "Three retries"`, "Not chosen: \"One retry\"", "To undo: change the setting back"} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
}

// A decided window of zero (or a config that names none) hides the list: the
// owner has said no record should wait there.
func TestDecidedWindowConfigurable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DecidedWindow = time.Hour
	record := requestMessage("rec", "decision", "p2p/lead__user", "lead", []string{reservedHuman}, "s1", requestNow.Add(-2*time.Hour))
	if err := json.Unmarshal([]byte(`{"decision":{"options":[{"label":"A"},{"label":"B"}],"decided":"A"}}`), &record.Context); err != nil {
		t.Fatal(err)
	}
	m := requestSnapshot(record).Project(cfg, requestNow, "test")
	if len(m.Inbox.Decided) != 0 {
		t.Fatalf("record outside the window shown: %+v", m.Inbox.Decided)
	}
}

// The page shows the decided rows with their overrule buttons between the
// waiting items and the mail lines.
func TestHTMLInboxShowsDecidedRecords(t *testing.T) {
	record := requestMessage("rec", "decision", "p2p/lead__user", "lead", []string{reservedHuman}, "s1", requestNow.Add(-2*time.Hour))
	if err := json.Unmarshal([]byte(`{"decision":{"options":[{"label":"Three retries"},{"label":"One retry"}],"decided":"Three retries","if_later":"change the setting back"}}`), &record.Context); err != nil {
		t.Fatal(err)
	}
	m := requestSnapshot(record).Project(DefaultConfig(), requestNow, "test")
	var out strings.Builder
	if err := WriteHTML(&out, m, ""); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"Decided without you", "chose", "Three retries", "Not chosen:", "To undo: change the setting back", "Overrule: choose One retry"} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
