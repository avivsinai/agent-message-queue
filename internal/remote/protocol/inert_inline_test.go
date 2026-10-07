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
		// Review of #972 r3: InertInline must call BoundReason itself. A raw
		// err.Error() with U+202E escaped the code span (the bidi override
		// survives outside one) and a "line1\n\n[x](https://e.x)" payload let
		// CommonMark block structure beat the inline span.
		{"bidi override raw", "bad\u202Ecode", "`badcode`"},
		{"newline and link", "line1\n\n[x](https://e.x)", "`line1 [x](https://e.x)`"},
		{"filters to empty", "\u202E\u200B", "`(no detail)`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InertInline(tc.in); got != tc.want {
				t.Fatalf("InertInline(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
