package board

import "testing"

// A full board prunes its oldest unheld history instead of stopping
// ingestion, and does not take that history back (board snapshot at 92% of
// its cap with ingestion about to stop, bead agent-message-queue-9df).
func TestPruneDropsOldestUnheldHistory(t *testing.T) {
	message := func(key, created string) Message {
		return Message{Key: key, ID: key, Root: "/r", Created: created, EvidenceHash: "hash-" + key}
	}
	s := Snapshot{
		Generation:   7,
		Messages:     map[string]Message{"old-held": message("old-held", "2026-01-01T00:00:00Z"), "old": message("old", "2026-01-02T00:00:00Z"), "new": message("new", "2026-01-03T00:00:00Z")},
		Observations: map[string]MessageObservation{"old": {}, "new": {}},
		Dismissed:    map[string]bool{"old": true},
		Tasks:        map[string]Task{"task-1": {ID: "task-1", Attachments: []string{"old-held"}}},
	}
	if !s.pruneHistory(1) {
		t.Fatal("nothing pruned")
	}
	if _, ok := s.Messages["old"]; ok || s.Dismissed["old"] {
		t.Fatal("oldest unheld message kept")
	}
	if _, ok := s.Messages["old-held"]; !ok {
		t.Fatal("attached message pruned")
	}
	if _, ok := s.Messages["new"]; !ok {
		t.Fatal("pruned past the requested bytes")
	}
	if s.RetiredEvidence["hash-old"] != 7 || len(s.RetiredEvidence) != 1 {
		t.Fatalf("retired evidence = %v", s.RetiredEvidence)
	}
	if !s.belowFloor(message("old", "2026-01-02T00:00:00Z")) || s.belowFloor(message("newer", "2026-01-02T00:00:01Z")) {
		t.Fatalf("floor %v does not separate pruned from new history", s.HistoryFloor)
	}
}
