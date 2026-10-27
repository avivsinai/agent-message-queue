package board

import (
	"reflect"
	"testing"
	"time"
)

// The next-action policy orders existing actionable records without turning
// marked work or an epic into another task. Recorded priorities rank backlog.
func TestNextActionsUseRecordedStateAndPriority(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	known := map[string]string{}
	bead := func(id string, state State, priority string) Item {
		known[id] = "open"
		ref, _ := Bead(id)
		item := Item{Ref: ref, State: state, Structural: true, Reason: Reason{Since: now}}
		if priority != "" {
			item.Evidence = []Evidence{{Field: "priority", Value: priority}}
		}
		return item
	}
	active := bead("demo-active", StateActive, "P0")
	epic := bead("demo-epic", StateOpenUnowned, "P0")
	epic.Evidence = append(epic.Evidence, Evidence{Field: "issue_type", Value: "epic"})
	parent := bead("demo-parent", StateQueued, "P0")
	parent.Evidence = append(parent.Evidence, Evidence{Field: "issue_type", Value: "feature"})
	child := bead("demo-child", StateQueued, "P3")
	child.Relations = []Relation{{Kind: RelationRelated, To: parent.Ref, Basis: "parent-child"}}
	waitRef, _ := ClaudeSession("aaaaaaaa-0000-4000-8000-000000000001")
	waiting := Item{Ref: waitRef, State: StateNeedsHuman, Reason: Reason{Since: now}}
	blocked := bead("demo-blocked", StateBlocked, "")
	blocked.Owner = Owner{Kind: OwnerAgent, Handle: "alpha"}
	blocked.WaitsOn = []Owner{{Kind: OwnerAgent, Handle: "beta"}}
	ref, _ := Obligation("s1/request/beta")
	request := Item{Ref: ref, State: StateRequested, Structural: true,
		Owner:      Owner{Kind: OwnerAgent, Handle: "beta", Session: "s1"},
		WaitsOn:    []Owner{{Kind: OwnerAgent, Handle: "beta", Display: "beta"}},
		Reason:     Reason{Since: now.Add(-time.Hour)},
		Obligation: &ObligationFacts{Thread: "p2p/alpha__beta", OwnerSession: "s1"}}
	items := []Item{active, epic, blocked, parent, child,
		bead("demo-z", StateQueued, "P1"), bead("demo-a", StateOpenUnowned, "P1"),
		bead("demo-p2", StateOpenUnowned, "P2"), bead("demo-unknown", StateOpenUnowned, "")}
	model := Build(Input{Results: []NamedResult{
		{Name: "beads", Result: Result{Status: StatusOK, Items: items}},
		{Name: "claude-permissions", Result: Result{Status: StatusOK, Items: []Item{waiting}}},
		{Name: "amq", Result: Result{Status: StatusOK, Items: []Item{request},
			AMQ: &AMQContext{Sessions: []SessionInfo{{Name: "s1", Path: "/fixture/s1"}}}}}},
		Known: known, Now: now, Config: DefaultConfig()})
	want := []NextAction{
		{CardID: waitRef.String(), Reason: "Needs your input"},
		{CardID: ref.String(), Reason: "Reply due from beta"},
		{CardID: "bead:demo-blocked", Reason: "Recorded blocker"},
		{CardID: "bead:demo-a", Reason: "Open; not started"},
		{CardID: "bead:demo-z", Reason: "Open; not started"},
		{CardID: "bead:demo-p2", Reason: "Open; not started"},
		{CardID: "bead:demo-child", Reason: "Open; not started"},
		{CardID: "bead:demo-unknown", Reason: "Open; not started"},
	}
	if !reflect.DeepEqual(model.Next, want) {
		t.Fatalf("next = %+v, want %+v", model.Next, want)
	}
	for _, column := range model.Columns {
		if column.ID != ColumnUnclaimed {
			continue
		}
		for _, card := range column.Cards {
			if card.ID == parent.Ref.String() {
				return
			}
		}
	}
	t.Fatal("the feature parent is missing from Backlog")
}
