package board

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// Review of the disk bounds: compaction moved journal.jsonl away before the
// new base was written, so a crash there read back an older generation.
func TestCompactionCrashBeforeNewBaseKeepsPublishedGeneration(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	gens := []string{`{"schema":"s","generation":1,"n":1}`, `{"schema":"s","generation":2,"n":2}`, `{"schema":"s","generation":3,"n":3}`}
	var journal []byte
	for i := 1; i < len(gens); i++ {
		ops, err := diffRaw(json.RawMessage(gens[i-1]), json.RawMessage(gens[i]), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		line, _ := json.Marshal(journalRecord{Generation: uint64(i + 1), Ops: ops})
		journal = append(append(journal, line...), '\n')
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "snapshot.json"), []byte(gens[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, journalName), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	// The crash point: the recovery copy is kept, the new base is not written.
	if err := store.keepBackup(); err != nil {
		t.Fatal(err)
	}
	backup := Store{Dir: t.TempDir()}
	for from, to := range map[string]string{"snapshot.backup.json": "snapshot.json", backupJournalName: journalName} {
		data, err := os.ReadFile(filepath.Join(store.Dir, from))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(backup.Dir, to), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, s := range map[string]Store{"published": store, "recovery copy": backup} {
		doc, err := s.readDocument()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got struct{ Generation uint64 }
		if err = json.Unmarshal(doc.data, &got); err != nil || got.Generation != 3 {
			t.Fatalf("%s reads generation %d (%v), want 3", name, got.Generation, err)
		}
	}
}
