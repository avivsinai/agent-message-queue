package board

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// askSnapshot is one current bead task plus n todos. The first todo names
// the task's bead inside a branch name, which relates it to the task without
// attaching it; each body is long and carries a sentinel.
func askSnapshot(n int, body string) Snapshot {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	bead, _ := Bead("demo-abc")
	alias := "beads:/work/.beads:demo-abc"
	fact := Item{Ref: bead, Source: "beads", Title: "Deliver feature", State: StateActive, Structural: true, Owner: Owner{Kind: OwnerAgent, Handle: "worker"}}
	s := Snapshot{
		Input:    Input{Dir: "/work", Known: map[string]string{"demo-abc": "in_progress"}, Results: []NamedResult{{Name: "beads", Result: Result{Status: StatusOK, ObservedAt: now}}}},
		Tasks:    map[string]Task{"task-1": {ID: "task-1", Title: "Deliver feature", Authority: alias, Aliases: []string{alias}}},
		Facts:    map[string]AliasFact{alias: {Alias: alias, Fact: fact, ObservedAt: now}},
		Messages: map[string]Message{},
	}
	for i := range n {
		id := fmt.Sprintf("msg-%04d", i)
		subject := fmt.Sprintf("Follow-up %d", i)
		if i == 0 {
			subject = "Check feat/demo-abc-parser before release"
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

// A request that names a current task's bead id shows on that card as
// related context and names the task on its own row, without changing tasks
// or counts.
func TestRequestThatNamesABeadShowsOnItsTask(t *testing.T) {
	m := askSnapshot(1, "Please look at it.").Project(DefaultConfig(), time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), "test")
	var cards []Card
	for _, column := range m.Columns {
		cards = append(cards, column.Cards...)
	}
	if len(cards) != 1 || len(m.Catalogue) != 1 || requestCount(m) != 1 {
		t.Fatalf("cards=%d catalogue=%d requests=%d, want 1 each", len(cards), len(m.Catalogue), requestCount(m))
	}
	if related := cards[0].Related; len(related) != 1 || related[0].Ref.ID != "/work#msg-0000" {
		t.Fatalf("card related = %+v, want the one ask", related)
	}
	text, page := renderFaces(t, m)
	for face, out := range map[string]string{"text": text, "html": page} {
		if !strings.Contains(out, "1 request names this task") || !strings.Contains(out, "amq thread --id p2p/alpha__user") {
			t.Errorf("%s card lacks the request count or its thread command", face)
		}
	}
	if !strings.Contains(text, "mentions task: Deliver feature (task-1)") || !strings.Contains(page, "“Deliver feature” (task-1)") {
		t.Error("the request row does not name its task")
	}
}

// Installed amq-board 0.117.0 on a real machine, 2026-10-29: the default
// text view was 7,930 lines (2.77 MB) and the HTML 15.4 MB (Requests view
// 8.3 MB) because every unresolved ask carried its full body. Default views
// hold no body and stay the same size as history grows; --json keeps every
// body.
func TestDefaultViewsStayBoundedWithoutMessageBodies(t *testing.T) {
	const sentinel = "full-body-sentinel"
	body := strings.Repeat(sentinel+" ", 40)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	small := askSnapshot(40, body).Project(DefaultConfig(), now, "test")
	large := askSnapshot(80, body).Project(DefaultConfig(), now, "test")
	if requestCount(large) != 80 {
		t.Fatalf("requests = %d, want 80", requestCount(large))
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
		t.Errorf("text view grew %d bytes for 40 more requests", grow)
	}
	if grow := len(largePage) - len(smallPage); grow > 1024 {
		t.Errorf("html view grew %d bytes for 40 more requests", grow)
	}
}
