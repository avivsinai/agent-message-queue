package board

import (
	"sort"
	"strconv"
)

// Token is a related reference shown on a card. It never changes the card's
// membership or column.
type Token struct {
	Ref       Ref    `json:"ref"`
	Relation  string `json:"relation"`
	Basis     string `json:"basis"`
	Inferred  bool   `json:"inferred"`
	Ambiguous bool   `json:"ambiguous,omitempty"`
	Closed    bool   `json:"closed,omitempty"`
	Count     int    `json:"count,omitempty"`
	// Action is the context command for an amq-message token: it reads the
	// ask's exact thread.
	Action *Action `json:"action,omitempty"`
}

// ResolveMentions turns each item's candidate tokens into related references
// when the token is a verified bead id (design 4.2). known maps every
// verified bead id to its status.
func ResolveMentions(items []Item, known map[string]string) []Item {
	out := make([]Item, len(items))
	for i, it := range items {
		seen := map[Ref]bool{it.Ref: true}
		for _, r := range it.Relations {
			seen[r.To] = true
		}
		for _, m := range it.Mentions {
			id := ""
			for _, c := range BeadCandidates(m.Token) {
				if _, ok := known[c]; ok {
					id = c
					break
				}
			}
			ref, ok := Bead(id)
			if !ok || seen[ref] {
				continue
			}
			seen[ref] = true
			it.Relations = append(it.Relations, Relation{Kind: RelationRelated, To: ref, Basis: m.Basis, Inferred: true})
		}
		out[i] = it
	}
	return out
}

// group is one card's members before placement.
type group struct {
	items     []Item
	ambiguous map[Ref]bool
}

// Link builds cards as the connected components of same-work relations only.
// A same-work relation merges only when its target resolves to exactly one
// item and exactly one item claims it; otherwise every claimant shows the
// target as an ambiguous related reference (design 4.3).
func Link(items []Item) []group {
	byRef := map[Ref][]int{}
	for i, it := range items {
		byRef[it.Ref] = append(byRef[it.Ref], i)
	}
	claimants := map[Ref][]int{}
	for i, it := range items {
		for _, r := range it.Relations {
			if r.Kind == RelationSameWork {
				claimants[r.To] = append(claimants[r.To], i)
			}
		}
	}
	parent := make([]int, len(items))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	ambiguous := map[int]map[Ref]bool{}
	for ref, cs := range claimants {
		targets := byRef[ref]
		if len(targets) == 1 && len(cs) == 1 {
			a, b := find(cs[0]), find(targets[0])
			if a != b {
				parent[a] = b
			}
			continue
		}
		if len(targets) == 0 && len(cs) == 1 {
			continue
		}
		for _, c := range cs {
			if ambiguous[c] == nil {
				ambiguous[c] = map[Ref]bool{}
			}
			ambiguous[c][ref] = true
		}
	}
	roots := map[int]*group{}
	var order []int
	for i := range items {
		r := find(i)
		g := roots[r]
		if g == nil {
			g = &group{ambiguous: map[Ref]bool{}}
			roots[r] = g
			order = append(order, r)
		}
		g.items = append(g.items, items[i])
		for ref := range ambiguous[i] {
			g.ambiguous[ref] = true
		}
	}
	out := make([]group, 0, len(order))
	for _, r := range order {
		out = append(out, *roots[r])
	}
	return out
}

// tokens lists a card's related references: every relation whose target is
// not a member of the card, plus one "same thread" token per obligation
// thread shared with obligations on other cards.
func (g group) tokens(known map[string]string, threadSize map[Ref]int) []Token {
	members := map[Ref]bool{}
	for _, it := range g.items {
		members[it.Ref] = true
	}
	seen := map[string]bool{}
	var out []Token
	for _, it := range g.items {
		for _, r := range it.Relations {
			if members[r.To] {
				continue
			}
			key := r.To.String() + "\x00" + r.Basis
			if seen[key] {
				continue
			}
			seen[key] = true
			t := Token{Ref: r.To, Relation: string(r.Kind), Basis: r.Basis, Inferred: r.Inferred, Ambiguous: g.ambiguous[r.To]}
			if r.To.Kind == RefBead && known[r.To.ID] == "closed" {
				t.Closed = true
			}
			out = append(out, t)
		}
		if it.Thread != nil && threadSize[*it.Thread] > 1 && !seen[it.Thread.String()] {
			seen[it.Thread.String()] = true
			others := threadSize[*it.Thread] - countThread(g.items, *it.Thread)
			if others > 0 {
				out = append(out, Token{
					Ref:      *it.Thread,
					Relation: string(RelationRelated),
					Basis:    "same thread as " + strconv.Itoa(others) + " other obligation" + plural(others),
					Count:    others,
				})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref.String() < out[j].Ref.String() })
	return out
}

func countThread(items []Item, thread Ref) int {
	n := 0
	for _, it := range items {
		if it.Thread != nil && *it.Thread == thread {
			n++
		}
	}
	return n
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
