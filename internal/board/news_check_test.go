package board

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// The request clock outlives the text and the plan-limit example card.
func TestIncidentIdentityIgnoresTextAndAge(t *testing.T) {
	base := Problem{Kind: ProblemSeatAtLimit, Session: "s1", Handle: "lead", CardID: "one", Text: "old"}
	other := base
	other.CardID, other.Text = "two", "later"
	if base.Incident() != other.Incident() || base.Incident() == "" {
		t.Fatalf("limit incident changed with example card or text: %q != %q", base.Incident(), other.Incident())
	}
	for _, kind := range []ProblemKind{ProblemReplyOverdue, ProblemNewsOverdue} {
		base = Problem{Kind: kind, Request: "r-1", Text: "old"}
		other = base
		other.Text = "later"
		if base.Incident() == "" || base.Incident() != other.Incident() {
			t.Fatalf("%s incident changed with text", kind)
		}
	}
}

// An overdue open ask with nothing from its owner is a floor notice. It
// survives without a completion deadline; the reducer's first reply retires it.
func TestNewsOverdueProjection(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-2 * time.Hour)
	m := requestMessage("news", "todo", "request", "lead", []string{"worker"}, "s1", opened)
	m.NewsBy = now.Add(-time.Hour).Format(time.RFC3339)
	model := requestSnapshot(m).Project(DefaultConfig(), now, "test")
	model.PublishedAt = now
	if len(model.Asks) != 1 || model.Asks[0].NewsBy.IsZero() {
		t.Fatalf("news-only ask lost: %+v", model.Asks)
	}
	got, err := Check(model, DefaultConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != ProblemNewsOverdue || got[0].Request != m.Key {
		t.Fatalf("problems = %+v", got)
	}
	reply := requestMessage("news-reply", "status", "request", "worker", []string{"lead"}, "s1", now.Add(-90*time.Minute), m.ID)
	model = requestSnapshot(m, reply).Project(DefaultConfig(), now, "test")
	model.PublishedAt = now
	got, err = Check(model, DefaultConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("announcement after the owner's reply: %+v", got)
	}
}

// Publication preserves the source's first-news clock; the projected reply
// clears the same ask on a later publication.
func TestNewsClockSurvivesPublication(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.StableTreeIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root, identity, "host-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	initial, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Second)
	a := Acquisition{ProjectID: initial.ProjectID, Input: Input{Results: []NamedResult{{Name: "amq-history", Result: Result{Status: StatusOK, ObservedAt: now}}}}}
	add := func(entry thread.Entry) {
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		a.Messages = append(a.Messages, MessageFromEntry(root, root, "", entry, evidenceDigest(raw)))
		a.Evidence = append(a.Evidence, raw)
	}
	add(thread.Entry{ID: "news", From: "lead", To: []string{"worker"}, Thread: "work", Subject: "first news", Kind: "todo", Created: now.Add(-time.Hour).Format(time.RFC3339), NewsBy: now.Add(-time.Minute).Format(time.RFC3339)})
	if err := store.Commit(ctx, a); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Messages[root+"#news"].NewsBy == "" {
		t.Fatal("publication dropped news_by")
	}
	now = state.PublishedAt
	problems, err := Check(state.Project(DefaultConfig(), now, "test"), DefaultConfig(), now)
	if err != nil || len(problems) != 1 || problems[0].Kind != ProblemNewsOverdue {
		t.Fatalf("news-only publication = %+v, %v", problems, err)
	}
	add(thread.Entry{ID: "reply", From: "worker", To: []string{"lead"}, Thread: "work", Subject: "started", Kind: "status", Created: now.Format(time.RFC3339), Refs: []string{"news"}})
	if err := store.Commit(ctx, a); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now = state.PublishedAt
	problems, err = Check(state.Project(DefaultConfig(), now, "test"), DefaultConfig(), now)
	if err != nil || len(problems) != 0 {
		t.Fatalf("published owner reply did not settle first news: %+v, %v", problems, err)
	}
}

func evidenceDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
