package board

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// PR #1202 review: unbounded source titles wrapped over the board. All
// sources share the renderer's control neutralization and title limit.
func TestTextBoundsSourceTitlesAndMarksInferredOwners(t *testing.T) {
	bead, _ := Bead("demo-a1")
	goal, _ := CodexGoal("019f0000-0000-7000-8000-000000000001")
	bad := "\x1b[31m" + strings.Repeat("界", 300) + "\r\nforged line"
	var results []NamedResult
	for i, name := range []string{"beads", "codex-goals"} {
		ref := []Ref{bead, goal}[i]
		results = append(results, NamedResult{Name: name, Result: Result{Status: StatusOK, Items: []Item{{Ref: ref, Title: bad, State: StateActive, Owner: Owner{Kind: OwnerUnmapped}}}}})
	}
	model := Build(Input{Results: results, Config: DefaultConfig(), Now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)})
	var out strings.Builder
	if err := WriteText(&out, model); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.ContainsAny(text, "\x1b\r") || strings.Contains(text, "forged line") || !strings.Contains(text, "inferred (owner)") {
		t.Fatalf("unsafe title or missing owner qualifier: %q", text)
	}
	titles := 0
	for _, line := range strings.Split(text, "\n") {
		if title, ok := strings.CutPrefix(line, "  * "); ok {
			titles++
			if !utf8.ValidString(title) || utf8.RuneCountInString(title) != 160 || !strings.HasSuffix(title, "…") {
				t.Errorf("title not bounded safely: %q", title)
			}
		}
	}
	if titles != 2 {
		t.Fatalf("rendered %d source titles, want 2", titles)
	}
}

func TestSentence(t *testing.T) {
	long := strings.Repeat("word ", 60)
	for _, tc := range []struct{ name, in, want string }{
		// Finish review 2026-11-02: summaries cut at the limit ended on a dangling word.
		{"long sentence ends at a clause", "The owner approves a tool call from the phone, the terminal dialog closes at once, and the board shows the outcome beside the request without a second refresh.", "The owner approves a tool call from the phone, the terminal dialog closes at once…"},
		// Code review 2026-11-02: every first word was upper-cased ("IOS", "Npm").
		{"own case kept without a label", "iOS build breaks on the second run. More text.", "iOS build breaks on the second run."},
		{"bead label", "User outcome: the owner sees one sentence per card. Gap: none.", "The owner sees one sentence per card."},
		{"pr heading and bullets", "## Summary\n\n- Adds a `--port` flag to the [relay](http://x) so it listens elsewhere.\n- Docs.", "Adds a --port flag to the relay so it listens elsewhere."},
		{"semicolon", "Fix the clock; add a test", "Fix the clock"},
		{"label alone on its paragraph", "User outcome:\n\nThe owner sees one sentence.", "The owner sees one sentence."},
		{"bold label alone", "**User outcome:**\n\nThe owner sees one sentence.", "The owner sees one sentence."},
		{"empty label", "Gap: \n\nX marks it.", "X marks it."},
		{"html comment and closes line", "<!-- Describe the change -->\nCloses #12\n\nAdds the relay flag. More text.", "Adds the relay flag."},
		{"semicolon inside code", "Fix the `a; b` parsing. Later.", "Fix the a; b parsing."},
		{"bullets join", "- Adds a flag\n- Updates the docs", "Adds a flag; Updates the docs"},
		{"empty", "  \n", ""},
		{"long", long, "word" + strings.Repeat(" word", 22) + "…"},
	} {
		got := Sentence(tc.in)
		if got != tc.want {
			t.Errorf("%s: Sentence = %q, want %q", tc.name, got, tc.want)
		}
		if utf8.RuneCountInString(got) > SentenceLimit {
			t.Errorf("%s: %d runes", tc.name, utf8.RuneCountInString(got))
		}
	}
}

func TestTextShowsCardSummaryUnderTitle(t *testing.T) {
	bead, _ := Bead("demo-a1")
	m := Build(Input{Results: []NamedResult{{Name: "beads", Result: Result{Status: StatusOK, Items: []Item{{Ref: bead, Title: "Deliver", State: StateActive, Owner: Owner{Kind: OwnerAgent, Handle: "w"}, Evidence: []Evidence{{Field: FieldSummary, Text: "User outcome: the owner sees one line."}}}}}}}, Config: DefaultConfig(), Now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)})
	var out strings.Builder
	if err := WriteText(&out, m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  * Deliver\n    The owner sees one line.\n") {
		t.Fatalf("summary not under the title: %q", out.String())
	}
}
