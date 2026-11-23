package dialog

import "testing"

// wb7p: an approving word must never extend a grant beyond its narrowest
// offered scope, including when the session option comes first.
func TestApproveOptionUsesNarrowestScope(t *testing.T) {
	for _, row := range []struct {
		name    string
		options []Option
		want    int
	}{
		{name: "once first", options: []Option{{N: 1, Scope: ScopeOnce}}, want: 1},
		{name: "session first", options: []Option{{N: 1, Scope: ScopeSession}, {N: 2, Scope: ScopeOnce}}, want: 2},
		{name: "only always and deny", options: []Option{{N: 1, Scope: ScopeAlways}, {N: 2, Deny: true}}},
		{name: "turn before session", options: []Option{{N: 1, Scope: ScopeTurn}, {N: 2, Scope: ScopeSession}, {N: 3, Deny: true}}, want: 1},
		{name: "same scope keeps order", options: []Option{{N: 1, Scope: ScopeOnce}, {N: 2, Scope: ScopeOnce}}, want: 1},
		{name: "questions and old records keep first", options: []Option{{N: 1, Label: "Choice A"}, {N: 2, Label: "Choice B"}}, want: 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			req := Request{ID: "d-test", Options: row.options}
			got, ok := ApproveOption(req)
			if (got.N != row.want) || ok != (row.want != 0) {
				t.Fatalf("ApproveOption = %+v, %v; want %d", got, ok, row.want)
			}
			answer, answered := ParseAnswer(req, "yes")
			if answered != (row.want != 0) || answer.Option != row.want {
				t.Fatalf("yes = %+v, %v; want %d", answer, answered, row.want)
			}
		})
	}
}
