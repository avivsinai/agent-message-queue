package protocol

import "testing"

// Review of #972 r3: character filtering cannot make adapter text inert in
// the DM — the renderer (remark + remark-gfm) decodes HTML entities back
// into bidi overrides and autolinks URLs. InertInline wraps the text in a
// code span, where neither happens.
func TestInertInline(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"entity", "&#x202E;abc", "`&#x202E;abc`"},
		{"url", "see https://evil.example", "`see https://evil.example`"},
		{"mention", "@everyone", "`@everyone`"},
		{"plain backticks", "run `amq status`", "`` run `amq status` ``"},
		{"leading backtick", "`quoted", "`` `quoted ``"},
		{"trailing backtick", "quoted`", "`` quoted` ``"},
		{"backtick run", "a ``` b", "````a ``` b````"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InertInline(tc.in); got != tc.want {
				t.Fatalf("InertInline(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
