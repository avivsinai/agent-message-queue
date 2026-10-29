package board

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// askSnapshot is one current bead task plus n open todos. The first todo
// names the task's bead id; each body is long and carries a sentinel.
func askSnapshot(n int, body string) Snapshot {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	bead, _ := Bead("demo-a1b")
	alias := "beads:/work/.beads:demo-a1b"
	fact := Item{Ref: bead, Source: "beads", Title: "Deliver feature", State: StateActive, Structural: true, Owner: Owner{Kind: OwnerAgent, Handle: "worker"}}
	s := Snapshot{
		Input:    Input{Dir: "/work", Known: map[string]string{"demo-a1b": "in_progress"}, Results: []NamedResult{{Name: "beads", Result: Result{Status: StatusOK, ObservedAt: now}}}},
		Tasks:    map[string]Task{"task-1": {ID: "task-1", Title: "Deliver feature", Authority: alias, Aliases: []string{alias}}},
		Facts:    map[string]AliasFact{alias: {Alias: alias, Fact: fact, ObservedAt: now}},
		Messages: map[string]Message{},
	}
	for i := range n {
		id := fmt.Sprintf("msg-%04d", i)
		subject := fmt.Sprintf("Follow-up %d", i)
		if i == 0 {
			subject = "Check demo-a1b before release"
		}
		s.Messages["/work#"+id] = Message{Namespace: "/work", Key: "/work#" + id, Root: "/work", ID: id, Thread: "p2p/alpha__user", From: "alpha", To: []string{"user"}, Subject: subject, Kind: "todo", Body: body, Created: now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339Nano)}
	}
	return s
}

func renderFaces(t *testing.T, m Model) (string, string) {
	t.Helper()
	var text, page strings.Builder
	if err := WriteText(&text, m); err != nil {
		t.Fatal(err)
	}
	if err := WriteHTML(&page, m, "amq-board --html"); err != nil {
		t.Fatal(err)
	}
	return text.String(), page.String()
}

// An open ask that names a current task's bead id shows on that card and
// names the task on its own strip, without changing tasks or counts.
func TestOpenAskThatNamesABeadShowsOnItsTask(t *testing.T) {
	m := askSnapshot(1, "Please look at it.").Project(DefaultConfig(), time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), "test")
	var cards []Card
	for _, column := range m.Columns {
		cards = append(cards, column.Cards...)
	}
	if len(cards) != 1 || len(m.Catalogue) != 1 || len(m.Unresolved) != 1 {
		t.Fatalf("cards=%d catalogue=%d unresolved=%d, want 1 each", len(cards), len(m.Catalogue), len(m.Unresolved))
	}
	if related := cards[0].Related; len(related) != 1 || related[0].Ref.ID != "/work#msg-0000" {
		t.Fatalf("card related = %+v, want the one ask", related)
	}
	text, page := renderFaces(t, m)
	for face, out := range map[string]string{"text": text, "html": page} {
		if !strings.Contains(out, "1 open ask names this task") || !strings.Contains(out, "amq thread --id p2p/alpha__user") {
			t.Errorf("%s card lacks the ask count or its thread command", face)
		}
	}
	if !strings.Contains(text, "names task: Deliver feature (task-1)") || !strings.Contains(page, "Names task “Deliver feature” (task-1)") {
		t.Error("the ask strip does not name its task")
	}
}

// Installed amq-board 0.117.1 on a real machine, 2026-10-29: the default
// text view was 7,930 lines (2.77 MB) and the HTML 15.4 MB (Requests view
// 8.3 MB) because every unresolved ask carried its full body. Default views
// stay the same size as history grows and hold no body; --json keeps them.
func TestDefaultViewsStayBoundedWithoutMessageBodies(t *testing.T) {
	const sentinel = "full-body-sentinel"
	body := strings.Repeat(sentinel+" ", 800)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	small := askSnapshot(200, body).Project(DefaultConfig(), now, "test")
	large := askSnapshot(400, body).Project(DefaultConfig(), now, "test")
	if len(large.Unresolved) != 400 {
		t.Fatalf("unresolved = %d, want 400", len(large.Unresolved))
	}
	smallText, smallPage := renderFaces(t, small)
	largeText, largePage := renderFaces(t, large)
	for face, out := range map[string]string{"text": largeText, "html": largePage} {
		if strings.Contains(out, sentinel) {
			t.Errorf("%s view carries a message body", face)
		}
		if !strings.Contains(out, "amq-board --json") {
			t.Errorf("%s view does not point to --json", face)
		}
	}
	if grow := len(largeText) - len(smallText); grow > 1024 {
		t.Errorf("text view grew %d bytes for 200 more asks", grow)
	}
	if grow := len(largePage) - len(smallPage); grow > 1024 {
		t.Errorf("html view grew %d bytes for 200 more asks", grow)
	}
}
