package board

import (
	"testing"
	"time"
)

// Review of the disk bounds, round 1, finding 2: pruning held requests by a
// second rule forever; it follows the open obligations up to the horizon.
func TestPruneHoldsOpenObligationsWithinTheHorizon(t *testing.T) {
	now := time.Now().UTC()
	msg := func(name, kind, from, to string, ago time.Duration, refs ...string) Message {
		at := now.Add(-ago)
		id := at.Format("2006-01-02T15-04-05.000Z") + "_pid1_" + name
		return Message{Namespace: "ns", Copies: []MessageCopy{{Root: "r", Agents: []string{"user", "worker"}}}, Key: "ns#" + id, ID: id, Thread: "work", From: from, To: []string{to}, Kind: kind, Refs: refs, Created: at.Format(time.RFC3339Nano), EvidenceHash: name}
	}
	acked := msg("acked", "todo", "user", "worker", 3*time.Hour)
	ack := msg("ack", "status", "worker", "user", 2*time.Hour, acked.ID)
	stale := msg("stale", "todo", "user", "worker", prunedIDHorizon+24*time.Hour)
	note := msg("note", "status", "user", "worker", prunedIDHorizon+23*time.Hour, stale.ID)
	s := Snapshot{Generation: 1, PublishedAt: now, Messages: map[string]Message{acked.Key: acked, ack.Key: ack, stale.Key: stale, note.Key: note}}
	if !s.pruneHistory(1<<30, nil) {
		t.Fatal("nothing pruned")
	}
	kept := func(m Message) bool { _, ok := s.Messages[m.Key]; return ok }
	if !kept(acked) || !kept(ack) || kept(stale) || kept(note) {
		t.Fatalf("acked=%v ack=%v stale=%v note=%v, want the acknowledged todo and its reply kept and the todo past the horizon pruned", kept(acked), kept(ack), kept(stale), kept(note))
	}
}

// Review of the disk bounds, round 3, finding 2: a request whose parent was
// pruned read as an unresolved lineage, so the board closed what thread open
// still lists as open.
func TestRequestIntoAPrunedThreadStaysOpen(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-time.Hour)
	pruned := now.Add(-2*time.Hour).Format("2006-01-02T15-04-05.000Z") + "_pid1_gone"
	id := at.Format("2006-01-02T15-04-05.000Z") + "_pid1_ask"
	ask := Message{Namespace: "ns", Copies: []MessageCopy{{Root: "r", Agents: []string{"user", "worker"}}}, Key: "ns#" + id, ID: id, Thread: "work", From: "user", To: []string{"worker"}, Kind: "todo", Refs: []string{pruned}, Created: at.Format(time.RFC3339Nano), EvidenceHash: "ask"}
	s := Snapshot{Generation: 1, PublishedAt: now, PrunedIDs: map[string]time.Time{pruned: now.Add(-2 * time.Hour)}, Messages: map[string]Message{ask.Key: ask}}
	if !s.openRequests()[ask.Key] {
		t.Fatal("a request replying into a pruned thread is not open")
	}
}
