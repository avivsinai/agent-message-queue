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
