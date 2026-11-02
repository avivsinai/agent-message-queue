package board

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

var requestNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// requestMessage is one message held only in session's root of /base.
func requestMessage(id, kind, thread, from string, to []string, session string, at time.Time, refs ...string) Message {
	root := "/base/" + session
	return Message{Namespace: "/base", Key: "/base#" + id, Root: root, Session: session, Copies: []MessageCopy{{Root: root, Session: session, Agents: append([]string{from}, to...)}}, ID: id, Thread: thread, From: from, To: to, Subject: "subject " + id, Kind: kind, Body: "body " + id, Created: at.Format(time.RFC3339Nano), Refs: refs, EvidenceHash: digestValue(id)}
}

func requestSnapshot(messages ...Message) Snapshot {
	s := Snapshot{
		ProjectID: "project-test",
		Input:     Input{Dir: "/work", Results: []NamedResult{{Name: "amq", Result: Result{Status: StatusOK, AMQ: &AMQContext{Project: "proj"}}}, {Name: "amq-history", Result: Result{Status: StatusOK}}}},
		Tasks:     map[string]Task{},
		Facts:     map[string]AliasFact{},
		Messages:  map[string]Message{},
		Dismissed: map[string]bool{}, ActionCancelled: map[string]bool{},
	}
	for _, m := range messages {
		s.Messages[m.Key] = m
	}
	return s
}

// A direct reply (last ref, any thread or clock) and later same-thread
// activity are separate observations. A request counts once when any scoped
// recipient sent no direct reply; age partitions without completing, and a
// future or unreadable time is Undated.
func TestRequestObservationsSeparateDirectReplyFromLaterActivity(t *testing.T) {
	day := 24 * time.Hour
	s := requestSnapshot(
		requestMessage("ask", "todo", "work", "lead", []string{"worker", "reviewer"}, "s1", requestNow.Add(-2*day)),
		requestMessage("reply", "status", "elsewhere", "worker", []string{"lead"}, "s1", requestNow.Add(-3*day), "ask"),
		requestMessage("later", "status", "work", "worker", []string{"lead"}, "s1", requestNow.Add(-day)),
		requestMessage("nudge", "status", "work", "lead", []string{"reviewer"}, "s1", requestNow.Add(-day)),
		requestMessage("answered", "question", "work2", "lead", []string{"worker"}, "s1", requestNow.Add(-2*day)),
		requestMessage("answer", "answer", "work2", "worker", []string{"lead"}, "s1", requestNow.Add(-day), "answered"),
		requestMessage("old", "todo", "old", "lead", []string{"worker"}, "s1", requestNow.Add(-20*day)),
		requestMessage("future", "todo", "f", "lead", []string{"worker"}, "s1", requestNow.Add(day)),
	)
	unreadable := requestMessage("unreadable", "todo", "u", "lead", []string{"worker"}, "s1", requestNow)
	unreadable.Created = "yesterday"
	s.Messages[unreadable.Key] = unreadable
	v := s.RequestView(requestNow)
	if len(v.Recent) != 2 || len(v.Older) != 1 || v.Older[0].Message.ID != "old" || v.Older[0].Age != RequestOlder || len(v.Undated) != 2 {
		t.Fatalf("recent=%d older=%+v undated=%d, want 2 recent, the old todo older, and 2 undated", len(v.Recent), v.Older, len(v.Undated))
	}
	ask := v.Recent[0]
	if ask.Message.ID != "ask" || !ask.NoDirectReply || v.Recent[1].NoDirectReply {
		t.Fatalf("recent order or silence wrong: %+v", v.Recent)
	}
	worker, reviewer := ask.Recipients[0], ask.Recipients[1]
	if !reflect.DeepEqual(worker.DirectReplies, []string{"/base#reply"}) || !reflect.DeepEqual(worker.LaterMessages, []string{"/base#later"}) {
		t.Fatalf("worker observations = %+v", worker)
	}
	if !reviewer.Scoped || reviewer.Session != "s1" || !reviewer.LaterAssessed || len(reviewer.LaterMessages) != 0 || len(reviewer.DirectReplies) != 0 {
		t.Fatalf("reviewer observations = %+v", reviewer)
	}
	if v.NoDirectReply != 1 {
		t.Fatalf("headline count = %d, want the two-recipient request once", v.NoDirectReply)
	}
}

// Bare handles never become a guessed scope: a routed request whose
// recipient copy was not observed is not counted. A cross-session request is
// scoped to its delivery root. A request without a thread is still assessed
// for a direct reply, though not for later thread activity.
func TestRequestScopeComesFromRoutingAndUnknownIsNotCounted(t *testing.T) {
	hour := time.Hour
	unscoped := requestMessage("unscoped", "question", "x", "lead", []string{"worker"}, "s1", requestNow.Add(-hour))
	unscoped.ReplyTo = "lead@s1"
	routed := requestMessage("routed", "review_request", "y", "lead", []string{"worker"}, "s1", requestNow.Add(-hour))
	routed.ReplyTo = "lead@s1"
	routed.Copies = append(routed.Copies, MessageCopy{Root: "/base/s2", Session: "s2", Agents: []string{"worker"}})
	answer := requestMessage("answer", "review_response", "y", "worker", []string{"lead"}, "s2", requestNow, "routed")
	answer.ReplyTo = "worker@s2"
	s := requestSnapshot(unscoped, routed, answer, requestMessage("threadless", "todo", "", "lead", []string{"worker"}, "s1", requestNow.Add(-hour)))
	v := s.RequestView(requestNow)
	byID := map[string]Request{}
	for _, r := range v.Recent {
		byID[r.Message.ID] = r
	}
	if r := byID["unscoped"].Recipients[0]; r.Scoped || r.LaterAssessed || byID["unscoped"].NoDirectReply {
		t.Fatalf("unobserved routed recipient = %+v, want unknown", r)
	}
	if r := byID["threadless"].Recipients[0]; !r.Scoped || r.LaterAssessed || !byID["threadless"].NoDirectReply {
		t.Fatalf("threadless recipient = %+v, want scoped with no direct reply", r)
	}
	if r := byID["routed"].Recipients[0]; r.Session != "s2" || !reflect.DeepEqual(r.DirectReplies, []string{"/base#answer"}) || !reflect.DeepEqual(r.LaterMessages, []string{"/base#answer"}) {
		t.Fatalf("cross-session recipient = %+v, want s2 with its answer", r)
	}
	head, notes := requestHeadline(v)
	if v.NoDirectReply != 1 || v.UnknownScope != 1 || head != "1 recent request with no direct reply observed" || len(notes) != 1 {
		t.Fatalf("headline %q %v, counts %+v", head, notes, v)
	}
}

// Classification context is whole messages in priority order within eight
// messages and 16 KiB: the direct reply survives, nothing is clipped, and
// omissions are counted. A request too large alone is unclassifiable.
func TestClassifyContextKeepsWholeDirectReplies(t *testing.T) {
	ask := requestMessage("ask", "todo", "work", "lead", []string{"worker"}, "s1", requestNow.Add(-time.Hour))
	reply := requestMessage("reply", "status", "work", "worker", []string{"lead"}, "s1", requestNow, "ask")
	reply.Body = strings.Repeat("reply ", 500)
	messages := []Message{ask, reply}
	for _, id := range []string{"l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8"} {
		m := requestMessage(id, "status", "work", "lead", []string{"worker"}, "s1", requestNow.Add(-30*time.Minute))
		m.Body = strings.Repeat(id+" ", 700)
		messages = append(messages, m)
	}
	x := newRequestIndex(requestSnapshot(messages...))
	got, omitted, unclassifiable := x.classifyContext(ask.Key)
	size, _ := json.Marshal(got)
	if unclassifiable != "" || len(got) > requestContextMessages || len(size) > requestContextBytes || got[0].Role != "request" || got[1].Key != reply.Key || got[1].Body != reply.Body || omitted != len(messages)-len(got) || omitted == 0 {
		t.Fatalf("context: %d messages, %d bytes, %d omitted, roles %v", len(got), len(size), omitted, got)
	}
	for _, m := range got {
		if m.Body != x.s.Messages[m.Key].Body {
			t.Fatalf("%s was clipped", m.Key)
		}
	}
	ask.Body = strings.Repeat("x", requestContextBytes)
	x = newRequestIndex(requestSnapshot(ask))
	if got, _, unclassifiable := x.classifyContext(ask.Key); len(got) != 0 || unclassifiable == "" {
		t.Fatal("an oversized request was not unclassifiable")
	}
}

// classify export reads; import writes through the store's publication,
// keyed by message, evidence digest and classifier. A forged quote, a
// changed value and a stale digest are refused; new evidence outside the
// supplied slice makes the request current for classification again.
func TestClassifyRoundTripPersistsOpinionWithoutChangingFacts(t *testing.T) {
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.StableTreeIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(base, identity, "host-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	initial, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	acquisition := Acquisition{ProjectID: initial.ProjectID, Input: Input{Results: []NamedResult{{Name: "amq-history", Result: Result{Status: StatusOK, ObservedAt: now}}}}}
	add := func(id, kind, from, to, body string, at time.Time, refs ...string) {
		entry := thread.Entry{ID: id, From: from, To: []string{to}, Thread: "work", Subject: "subject " + id, Kind: kind, Body: body, Created: at.Format(time.RFC3339Nano), Refs: refs}
		raw, _ := json.Marshal(entry)
		sum := sha256.Sum256(raw)
		acquisition.Messages = append(acquisition.Messages, MessageFromEntry(base, base, "", entry, hex.EncodeToString(sum[:])))
		acquisition.Evidence = append(acquisition.Evidence, raw)
	}
	add("ask", "todo", "lead", "worker", "Please fix the parser.", now.Add(-time.Hour))
	add("reply", "status", "worker", "lead", "Done: merged the parser fix.", now.Add(-50*time.Minute), "ask")
	for i, id := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		add(id, "status", "lead", "worker", "Progress note "+id, now.Add(time.Duration(i-40)*time.Minute))
	}
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const classifier = "test-model/requests-v1"
	batch, err := state.Classify(classifier, 8, now)
	if err != nil || len(batch.Candidates) != 1 || len(batch.Candidates[0].Messages) != 8 || batch.Candidates[0].Omitted != 0 {
		t.Fatalf("export = %+v, %v", batch, err)
	}
	candidate := batch.Candidates[0]
	before := state.Project(DefaultConfig(), now, "test")
	labels := func(value, quote string) LabelBatch {
		entry := LabelEntry{MessageKey: candidate.MessageKey, EvidenceDigest: candidate.EvidenceDigest, Value: value}
		if quote != "" {
			entry.Quote = &RequestQuote{MessageKey: base + "#reply", Field: "body", Text: quote}
		}
		return LabelBatch{Schema: LabelsSchema, ProjectID: batch.ProjectID, Classifier: classifier, Labels: []LabelEntry{entry}}
	}
	if _, err = store.PutRequestLabels(ctx, labels("done", "merged the parser fix and released it")); err == nil {
		t.Fatal("a quote absent from the supplied message was accepted")
	}
	if added, err := store.PutRequestLabels(ctx, labels("done", "merged the parser fix")); err != nil || added != 1 {
		t.Fatalf("import added %d: %v", added, err)
	}
	imported, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := store.PutRequestLabels(ctx, labels("done", "merged the parser fix")); err != nil || added != 0 {
		t.Fatalf("identical reimport added %d: %v", added, err)
	}
	if same, _ := store.ReadContext(ctx); same.Generation != imported.Generation {
		t.Fatal("an unchanged import published a new generation")
	}
	if _, err = store.PutRequestLabels(ctx, labels("open", "merged the parser fix")); err == nil {
		t.Fatal("a different opinion replaced the recorded one")
	}
	labelled, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after := labelled.Project(DefaultConfig(), now, "test")
	r := after.Requests.Recent[0]
	if len(r.Opinions) != 1 || r.Opinions[0].Classifier != classifier || r.Opinions[0].Value != "done" || !reflect.DeepEqual(r.Recipients, before.Requests.Recent[0].Recipients) || after.Requests.NoDirectReply != before.Requests.NoDirectReply || len(after.Requests.Recent) != 1 {
		t.Fatalf("opinion changed facts or attention: %+v", after.Requests)
	}
	if next, _ := labelled.Classify(classifier, 8, now); len(next.Candidates) != 0 {
		t.Fatal("a request with a current opinion was exported again")
	}
	add("n7", "status", "lead", "worker", "One more note", now.Add(-time.Minute))
	if err = store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	grown, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := grown.Classify(classifier, 8, now)
	if len(again.Candidates) != 1 || again.Candidates[0].EvidenceDigest == candidate.EvidenceDigest || !reflect.DeepEqual(again.Candidates[0].Messages, candidate.Messages) || again.Candidates[0].Omitted != 1 {
		t.Fatalf("new evidence outside the slice did not renew the request: %+v", again)
	}
	if len(grown.Project(DefaultConfig(), now, "test").Requests.Recent[0].Opinions) != 0 {
		t.Fatal("an opinion on old evidence is shown as current")
	}
	if _, err = store.PutRequestLabels(ctx, labels("unclear", "")); err == nil {
		t.Fatal("a label for a stale digest was accepted")
	}
	fresh := labels("unclear", "")
	fresh.Labels[0].EvidenceDigest = again.Candidates[0].EvidenceDigest
	if _, err = store.PutRequestLabels(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if pruned, _ := store.ReadContext(ctx); len(pruned.RequestLabels) != 1 {
		t.Fatalf("labels = %+v, want the old-evidence label dropped", pruned.RequestLabels)
	}
}

// An attached task does not hide its request. Facts and attributed opinions
// render apart on every face, the quote is escaped, Older is its own view,
// and the old unresolved footer is gone.
func TestRequestsRenderFactsAndOpinionsSeparately(t *testing.T) {
	ask := requestMessage("ask", "todo", "work", "lead", []string{"worker"}, "s1", requestNow.Add(-time.Hour))
	old := requestMessage("old", "question", "past", "lead", []string{"worker"}, "s1", requestNow.Add(-30*24*time.Hour))
	old.Subject = "Archived parser question"
	s := requestSnapshot(ask, old)
	bead, _ := Bead("demo-abc")
	alias := "beads:/work/.beads:demo-abc"
	s.Tasks["task-1"] = Task{ID: "task-1", Title: "Deliver parser", Authority: alias, Aliases: []string{alias}, Attachments: []string{ask.Key}}
	s.Facts[alias] = AliasFact{Alias: alias, Fact: Item{Ref: bead, Source: "beads", Title: "Deliver parser", State: StateDone, Structural: true}}
	without := s.Project(DefaultConfig(), requestNow, "test")
	digest := without.Requests.Recent[0].EvidenceDigest
	label := RequestLabel{MessageKey: ask.Key, EvidenceDigest: digest, Classifier: "test-model/requests-v1", Value: "done", Quote: &RequestQuote{MessageKey: ask.Key, Field: "body", Text: "<script>alert(1)</script> shipped"}}
	s.RequestLabels = map[string]RequestLabel{RequestLabelKey(ask.Key, digest, label.Classifier): label}
	m := s.Project(DefaultConfig(), requestNow, "test")
	if len(m.Requests.Recent) != 1 || m.Requests.NoDirectReply != without.Requests.NoDirectReply || m.Requests.NoDirectReply != 1 {
		t.Fatalf("attachment or opinion changed attention: %+v", m.Requests)
	}
	if tasks := m.Requests.Recent[0].Tasks; len(tasks) != 1 || tasks[0].TaskID != "task-1" || tasks[0].State != StateDone {
		t.Fatalf("bound task = %+v", tasks)
	}
	text, page := renderFaces(t, m)
	for _, want := range []string{"1 recent request with no direct reply observed", "worker in s1: no direct reply observed; no later message in this thread", "from lead, 1h ago"} {
		if !strings.Contains(text, want) || !strings.Contains(page, want) {
			t.Errorf("text or html lacks %q", want)
		}
	}
	if !strings.Contains(text, "opinion: likely done · test-model/requests-v1") || !strings.Contains(page, `<span class="opinion-value">likely done</span> · test-model/requests-v1`) {
		t.Error("the opinion is not attributed to its classifier")
	}
	if strings.Contains(page, "<script>alert") || !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt; shipped") {
		t.Error("the opinion quote is not escaped")
	}
	if !strings.Contains(page, `class="opinion"`) || !strings.Contains(page, `class="request-facts"`) || !strings.Contains(page, `id="view-older-requests"`) || !strings.Contains(page, "Archived parser question") {
		t.Error("the page does not separate facts, opinions and older requests")
	}
	// Fix review: an import during a degraded sync dropped every other
	// message's label, because coverage is in each digest.
	oldDigest := m.Requests.Older[0].EvidenceDigest
	s.RequestLabels[RequestLabelKey(old.Key, oldDigest, label.Classifier)] = RequestLabel{MessageKey: old.Key, EvidenceDigest: oldDigest, Classifier: label.Classifier, Value: "unclear"}
	s.Input.Results[1].Result.Status = StatusDegraded
	degraded := newRequestIndex(s).request(ask.Key, requestNow).EvidenceDigest
	batch := LabelBatch{Schema: LabelsSchema, ProjectID: s.ProjectID, Classifier: label.Classifier, Labels: []LabelEntry{{MessageKey: ask.Key, EvidenceDigest: degraded, Value: "unclear"}}}
	if _, err := putLabels(&s, batch); err != nil {
		t.Fatal(err)
	}
	if _, kept := s.RequestLabels[RequestLabelKey(old.Key, oldDigest, label.Classifier)]; !kept || len(s.RequestLabels) != 2 {
		t.Fatalf("labels after a degraded import = %+v, want the other message's label kept and the batch message's old label dropped", s.RequestLabels)
	}
}

// Pre-merge review finding: one observed copy root was taken as proof that
// both parties live there, so after a cleanup removed the recipient's copy
// the replied request flipped to no direct reply. A handle is scoped only
// to the copy root whose agents name it.
func TestRequestScopeNeedsTheRootThatNamesTheHandle(t *testing.T) {
	ask := requestMessage("ask", "todo", "work", "lead", []string{"worker"}, "", requestNow.Add(-time.Hour))
	ask.Copies = []MessageCopy{{Root: "/base", Session: "", Agents: []string{"lead"}}, {Root: "/base/s1", Session: "s1", Agents: []string{"worker"}}}
	reply := requestMessage("reply", "status", "work", "worker", []string{"lead"}, "s1", requestNow, "ask")
	full := requestSnapshot(ask, reply).RequestView(requestNow)
	ask.Copies = ask.Copies[:1]
	pruned := requestSnapshot(ask, reply).RequestView(requestNow)
	if full.NoDirectReply != 0 || full.Recent[0].Recipients[0].Session != "s1" {
		t.Fatalf("both copies: %+v", full.Recent[0].Recipients)
	}
	if r := pruned.Recent[0].Recipients[0]; r.Scoped || pruned.NoDirectReply != 0 || pruned.UnknownScope != 1 {
		t.Fatalf("recipient copy pruned: %+v, headline %d, want unknown scope", r, pruned.NoDirectReply)
	}
}

// Pre-merge review finding: the digest covered only the request's own
// scopes, so a reply sender's scope change flipped the direct-reply
// observation while a label stayed current.
func TestDigestCoversReplySenderScope(t *testing.T) {
	ask := requestMessage("ask", "todo", "work", "lead", []string{"worker"}, "s1", requestNow.Add(-time.Hour))
	reply := requestMessage("reply", "status", "work", "worker", []string{"lead"}, "s1", requestNow, "ask")
	a := requestSnapshot(ask, reply).RequestView(requestNow).Recent[0]
	reply.Copies = []MessageCopy{{Root: "/base", Session: "", Agents: []string{"worker"}}, {Root: "/base/s1", Session: "s1", Agents: []string{"worker", "lead"}}}
	b := requestSnapshot(ask, reply).RequestView(requestNow).Recent[0]
	if len(a.Recipients[0].DirectReplies) != 1 || len(b.Recipients[0].DirectReplies) != 0 || a.EvidenceDigest == b.EvidenceDigest {
		t.Fatalf("replies %v -> %v, digest changed %v", a.Recipients[0].DirectReplies, b.Recipients[0].DirectReplies, a.EvidenceDigest != b.EvidenceDigest)
	}
}
