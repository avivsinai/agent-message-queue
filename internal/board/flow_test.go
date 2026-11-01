package board

import (
	"testing"
	"time"
)

// Every current card of a published snapshot lands in one Flow stage. A PR
// that a bead claims as the same work rides on the bead's card; other PRs
// are their own cards, and a review request flags its card Needs you.
func TestFlowStagesFromSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := Snapshot{Tasks: map[string]Task{}, Facts: map[string]AliasFact{}, Messages: map[string]Message{}}
	bead := func(task, id string, state State, since time.Time, relations ...Relation) {
		ref, _ := Bead(id)
		alias := "beads:/work/.beads:" + id
		fact := Item{Source: "beads", Namespace: "/work/.beads", Ref: ref, Title: id, State: state, Structural: true,
			Owner: Owner{Kind: OwnerAgent, Handle: "worker"}, Relations: relations, Reason: Reason{Field: "status", Since: since}}
		if state == StateOpenUnowned {
			fact.Owner = Owner{Kind: OwnerNone}
		}
		if state == StateDone {
			fact.Evidence = []Evidence{{Field: "closed_at", At: since}}
		}
		s.Tasks[task] = Task{ID: task, Title: id, Authority: alias, Aliases: []string{alias}}
		s.Facts[alias] = AliasFact{Alias: alias, Fact: fact, ObservedAt: now}
	}
	pr := func(n int, state State, evidence ...Evidence) Item {
		ref, _ := GitHubRepoRef("acme/app", n)
		return Item{Ref: ref, Title: "PR", State: state, Structural: true, Owner: Owner{Kind: OwnerAgent, Handle: "worker"},
			Evidence: append([]Evidence{{Field: "type", Value: ForgePullRequest}, {Field: "checks", Value: "FAILURE"}}, evidence...)}
	}
	linked := pr(10, StateActive)
	draft := pr(11, StateActive, Evidence{Field: "isDraft", Value: "true"})
	review := pr(12, StateNeedsHuman)
	review.WaitsOn = []Owner{{Kind: OwnerHuman, Display: "owner"}}

	bead("task-progress", "demo-1", StateActive, now.Add(-time.Hour), Relation{Kind: RelationSameWork, To: linked.Ref, Basis: "external_ref"})
	bead("task-blocked", "demo-2", StateBlocked, now.Add(-time.Hour))
	bead("task-queued", "demo-3", StateQueued, now.Add(-time.Hour))
	bead("task-unowned", "demo-4", StateOpenUnowned, now.Add(-time.Hour))
	bead("task-recent", "demo-5", StateDone, now.Add(-48*time.Hour))
	bead("task-old", "demo-6", StateDone, now.Add(-30*24*time.Hour))
	s.Input.Results = []NamedResult{{Name: "github", Result: Result{Status: StatusOK, ObservedAt: now, Items: []Item{linked, draft, review}}}}

	m := s.Project(DefaultConfig(), now, "test")
	stages := map[string]Stage{}
	for _, v := range m.Flow {
		for _, id := range v.CardIDs {
			stages[id] = v.ID
		}
	}
	want := map[string]Stage{
		"task-progress": StageInReview, "task-blocked": StageBlocked, "task-queued": StageReady,
		"task-unowned": StageBacklog, "task-recent": StageShipped,
		draft.Ref.String(): StageInProgress, review.Ref.String(): StageInReview,
	}
	if len(stages) != len(want) {
		t.Errorf("Flow holds %v, want %v", stages, want)
	}
	for id, stage := range want {
		if stages[id] != stage {
			t.Errorf("%s is in %q, want %q", id, stages[id], stage)
		}
	}
	cards := m.FlowCards()
	progress := cards["task-progress"]
	if len(progress.Items) != 2 || progress.Items[1].Ref != linked.Ref || progress.CI != CIRed {
		t.Errorf("the same-work PR did not ride on its bead card: items=%v ci=%q", progress.Items, progress.CI)
	}
	if flags := cards[review.Ref.String()].Flags; len(flags) != 1 || flags[0] != FlagNeedsYou {
		t.Errorf("the review-requested PR has flags %v, want Needs you", flags)
	}
}
