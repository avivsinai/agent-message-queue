package codex

import "testing"

// Test2ptIsLostStateStatus verifies the shared lost-state status helper.
// If a new lost-track status is added, it must be added here AND the helper
// — both threadStatus and the lost-state gate use the same source of truth.
// Bead agent-message-queue-2pt.
func Test2ptIsLostStateStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"notLoaded", true},
		{"systemError", true},
		{"idle", false},
		{"active", false},
		{"unknown", false},
		{"", false},
	}
	for _, c := range cases {
		got := isLostStateStatus(c.status)
		if got != c.want {
			t.Errorf("isLostStateStatus(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

// Test2ptThreadStatusUsesSharedHelper verifies threadStatus delegates to
// isLostStateStatus for the unknown mapping (not a separate literal list).
func Test2ptThreadStatusUsesSharedHelper(t *testing.T) {
	// notLoaded and systemError must both map to "unknown" via the helper
	if ts := threadStatus("notLoaded"); ts != "unknown" {
		t.Fatalf("threadStatus(notLoaded) = %q, want unknown", ts)
	}
	if ts := threadStatus("systemError"); ts != "unknown" {
		t.Fatalf("threadStatus(systemError) = %q, want unknown", ts)
	}
	// idle and active are NOT lost-state
	if ts := threadStatus("idle"); ts != "idle" {
		t.Fatalf("threadStatus(idle) = %q, want idle", ts)
	}
	if ts := threadStatus("active"); ts != "busy" {
		t.Fatalf("threadStatus(active) = %q, want busy", ts)
	}
}
