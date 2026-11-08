package board

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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

// Review of the board's idle disk cost: every refresh restamped each source
// fact's observed_at and wrote back every source item list whose length
// changed, so an idle refresh journaled the facts and lists again.
func TestIdleCommitWritesOnlyWhatChanged(t *testing.T) {
	home := t.TempDir()
	goal := Item{Ref: Ref{Kind: "codex-goal", ID: "119f0000-0000-4000-8000-000000000001"}, Container: "019f0000-0000-7000-8000-000000000001", Namespace: home, Title: "Deliver Codex goal", State: StateActive, Structural: true}
	obligation := func(id string) Item {
		return Item{Ref: Ref{Kind: "amq-obligation", ID: id}, Title: "Answer " + id, State: StateRequested, Structural: true}
	}
	refresh := func(at time.Time, complete bool, obligations ...Item) Input {
		return Input{Results: []NamedResult{
			{Name: "codex-goals", Result: Result{Status: StatusOK, ObservedAt: at, Items: []Item{goal}, NativeScopes: map[string]NativeScope{home: {Complete: complete}}}},
			{Name: "amq", Result: Result{Status: StatusOK, ObservedAt: at, Items: obligations}},
		}}
	}
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := first.Add(time.Minute)
	for _, row := range []struct {
		name          string
		before, after Input
		// factStamped allows the one change an incomplete scope makes.
		factStamped bool
	}{
		{name: "unchanged fact in a complete scope", before: refresh(first, true), after: refresh(later, true)},
		{name: "unchanged fact in an incomplete scope", before: refresh(first, false), after: refresh(later, false), factStamped: true},
		{name: "one item removed", before: refresh(first, true, obligation("a"), obligation("b")), after: refresh(later, true, obligation("b"))},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			identity, err := fsq.StableTreeIdentity(base)
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(base, identity, "host-test")
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Init(ctx); err != nil {
				t.Fatal(err)
			}
			initial, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: row.before}); err != nil {
				t.Fatal(err)
			}
			before, err := store.readDocument()
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: row.after}); err != nil {
				t.Fatal(err)
			}
			after, err := store.readDocument()
			if err != nil {
				t.Fatal(err)
			}
			ops, err := diffRaw(before.data, after.data, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range ops {
				p := op.Path
				clock := p[0] == "generation" || p[0] == "published_at" ||
					len(p) == 5 && p[0] == "input" && p[1] == "Results" && p[3] == "Result" && p[4] == "ObservedAt" ||
					row.factStamped && p[0] == "source_facts" && p[len(p)-1] == "observed_at"
				if !clock {
					t.Fatalf("idle refresh rewrote %v", p)
				}
			}
			state, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if got := state.Catalogue()[0].Facts[0].ObservedAt; !got.Equal(later) {
				t.Fatalf("fact still reported reads as checked at %v, want %v", got, later)
			}
		})
	}
}
