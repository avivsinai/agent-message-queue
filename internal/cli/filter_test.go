package cli

import "testing"

func TestFilterMessages_LabelAll(t *testing.T) {
	items := []listItem{
		{ID: "1", Labels: []string{"bug", "urgent"}},
		{ID: "2", Labels: []string{"bug"}},
		{ID: "3", Labels: []string{"urgent", "ops"}},
	}

	got := FilterMessages(items, FilterOptions{Labels: []string{"bug", "urgent"}})
	if len(got) != 1 {
		t.Fatalf("expected 1 item, got %d", len(got))
	}
	if got[0].ID != "1" {
		t.Fatalf("expected item 1, got %s", got[0].ID)
	}
}
