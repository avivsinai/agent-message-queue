package cli

import "testing"

func TestFilterMessages_NoFilters(t *testing.T) {
	items := []listItem{
		{ID: "1", From: "alice", Priority: "urgent", Kind: "question", Labels: []string{"bug"}},
		{ID: "2", From: "bob", Priority: "normal", Kind: "status", Labels: []string{"feature"}},
	}

	got := FilterMessages(items, FilterOptions{})
	if len(got) != len(items) {
		t.Fatalf("expected %d items, got %d", len(items), len(got))
	}
}

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
