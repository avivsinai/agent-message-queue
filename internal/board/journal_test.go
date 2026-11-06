package board

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A journal of leaf changes replays to the published document: keys added
// and removed, arrays edited in place or resized, scalars replaced.
func TestJournalReplaysToThePublishedDocument(t *testing.T) {
	gens := []string{
		`{"generation":1,"messages":{"a":{"subject":"x","to":["p","q"]},"b":{"subject":"y"}},"input":{"results":[{"name":"beads","observed_at":"t1","items":[1,2]}]}}`,
		`{"generation":2,"messages":{"a":{"subject":"x","to":["p","r"]},"c":{"subject":"z"}},"input":{"results":[{"name":"beads","observed_at":"t2","items":[1,2,3]}]}}`,
		`{"generation":3,"messages":{"a":{"subject":"x2","to":"p"}},"input":{"results":[]},"floor":"t0"}`,
	}
	var records []journalRecord
	for i := 1; i < len(gens); i++ {
		ops, err := diffRaw(json.RawMessage(gens[i-1]), json.RawMessage(gens[i]), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, journalRecord{Generation: uint64(i + 1), Ops: ops})
	}
	got, err := replayJournal(json.RawMessage(gens[0]), 1, records)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(gens[2]), &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("replayed %s, want %s", got, gens[2])
	}
}
