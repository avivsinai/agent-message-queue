package amit

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// fakeSource is an in-memory SessionSource; tests append entries and flip
// status directly.
type fakeSource struct {
	mu      sync.Mutex
	entries []ExtEntry
	status  string
	subs    []func(ExtEvent)
}

func (f *fakeSource) Subscribe(fn func(ExtEvent)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, fn)
	return func() {}
}

func (f *fakeSource) Entries() []ExtEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ExtEntry(nil), f.entries...)
}

func (f *fakeSource) Status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == "" {
		return "idle"
	}
	return f.status
}

func (f *fakeSource) add(e ExtEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	for _, fn := range f.subs {
		fn(ExtEvent{Type: e.Kind, ClientRef: e.ClientRef, Text: e.Text})
	}
}

func testKey(id string) requests.Key {
	return requests.Key{CreatorHost: "host1", TargetID: "amit", RequestID: id}
}

func newTestAttachment(t *testing.T, src *fakeSource) *Attachment {
	t.Helper()
	a, err := New("amit", "ep1", src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.now = func() time.Time { return time.Unix(0, 0) }
	return a
}

// TestSubmitConfirmedByUserMessageEntry pins the happy path: a submit is
// admitted only when the extension's user_message entry carrying the
// request ref lands; the entry lands before Submit checks, so the first
// Submit returns admitted.
func TestSubmitConfirmedByUserMessageEntry(t *testing.T) {
	src := &fakeSource{}
	a := newTestAttachment(t, src)
	// Pre-arm: the extension inbox write is test-injected, so emulate the
	// extension delivering by appending the entry as soon as the deliver
	// file would be observed. The deliver seam needs a dir; tests bypass it
	// by pre-inserting the run through a direct Submit with a prepared dir.
	a.deliverDir = t.TempDir()
	key := testKey("r1")

	// Emulate the extension being fast: it observes the inbox write and
	// appends the confirming user_message entry before Submit's immediate
	// re-check runs. New drained the tail at construction, so an entry
	// added after New is fresh correlation evidence.
	src.add(ExtEntry{Kind: "user_message", ClientRef: clientRef(key), Text: "hello"})
	admission, err := a.Submit(core.BoundRequest{Key: key, Epoch: "ep1", Input: protocol.SubmitInput{Text: "hello"}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !admission.Admitted {
		t.Fatalf("admission = %+v, want admitted (user_message entry confirmed the submit)", admission)
	}
	if admission.RunID == "" {
		t.Fatal("admitted run must have a run id")
	}
}

// TestSubmitUnconfirmedIsUncertainNeverRejected pins the evidence contract:
// without the extension's user_message entry the submit is uncertain (a
// non-nil error with the correlation bound), never a refusal code — pi's
// sendUserMessage swallows rejections, so a lost submit must not become a
// terminal rejected record.
func TestSubmitUnconfirmedIsUncertainNeverRejected(t *testing.T) {
	src := &fakeSource{}
	a := newTestAttachment(t, src)
	a.deliverDir = t.TempDir()
	admission, err := a.Submit(core.BoundRequest{Key: testKey("r2"), Epoch: "ep1", Input: protocol.SubmitInput{Text: "hi"}})
	if err == nil {
		t.Fatal("Submit without confirming entry must return an error (uncertain), want non-nil")
	}
	if admission.Code != "" {
		t.Fatalf("admission.Code = %q, want empty (no refusal code for an ambiguous submit)", admission.Code)
	}
	if admission.RunID == "" {
		t.Fatal("correlation must stay bound on the uncertain path (RunID empty)")
	}
}

// TestLookupUnknownForUnretainedKey pins the Amit evidence rule: a key the
// adapter retains nothing about is EvidenceUnknown, never EvidenceNone —
// a lost submit and a never-submitted key look identical to this adapter.
func TestLookupUnknownForUnretainedKey(t *testing.T) {
	src := &fakeSource{}
	a := newTestAttachment(t, src)
	ev, err := a.Lookup(testKey("never"), "ep1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ev.Class != core.EvidenceUnknown {
		t.Fatalf("evidence class = %q, want unknown (adapter cannot prove non-admission)", ev.Class)
	}
}

// TestAgentMessageCompletesRunFromSessionDiff pins completion: the
// agent_message entry after a confirmed submit moves the run to completed
// with the answer text as the retained result.
func TestAgentMessageCompletesRunFromSessionDiff(t *testing.T) {
	src := &fakeSource{}
	a := newTestAttachment(t, src)
	a.deliverDir = t.TempDir()
	key := testKey("r3")
	src.add(ExtEntry{Kind: "user_message", ClientRef: clientRef(key), Text: "q"})
	if _, err := a.Submit(core.BoundRequest{Key: key, Epoch: "ep1", Input: protocol.SubmitInput{Text: "q"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	src.add(ExtEntry{Kind: "agent_message", Text: "the answer"})
	ev, err := a.Lookup(key, "ep1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ev.Class != core.EvidenceHistoryTerminated || ev.State != protocol.StateCompleted {
		t.Fatalf("evidence = %+v state=%s, want history-terminated completed", ev.Class, ev.State)
	}
	if ev.Result == nil || ev.Result.Text != "the answer" {
		t.Fatalf("result = %+v, want the agent answer text", ev.Result)
	}
}

// TestAcknowledgeReleasesResult pins the ack contract: after
// AcknowledgeResult with the exact digest, Lookup reports EvidenceNone so
// the endpoint's ack replay converges; a wrong digest releases nothing.
func TestAcknowledgeReleasesResult(t *testing.T) {
	src := &fakeSource{}
	a := newTestAttachment(t, src)
	a.deliverDir = t.TempDir()
	key := testKey("r4")
	src.add(ExtEntry{Kind: "user_message", ClientRef: clientRef(key), Text: "q"})
	if _, err := a.Submit(core.BoundRequest{Key: key, Epoch: "ep1", Input: protocol.SubmitInput{Text: "q"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	src.add(ExtEntry{Kind: "agent_message", Text: "answer"})
	ev, _ := a.Lookup(key, "ep1")
	if ev.Result == nil {
		t.Fatal("want retained result before ack")
	}
	wrong := protocol.EvidenceDigest(&protocol.Result{Text: "other"})
	a.AcknowledgeResult(key, "ep1", wrong)
	ev2, _ := a.Lookup(key, "ep1")
	if ev2.Class == core.EvidenceNone {
		t.Fatal("wrong digest must not release the result")
	}
	a.AcknowledgeResult(key, "ep1", protocol.EvidenceDigest(ev.Result))
	ev3, _ := a.Lookup(key, "ep1")
	if ev3.Class != core.EvidenceNone {
		t.Fatalf("evidence class after ack = %q, want none (released)", ev3.Class)
	}
}

// TestCancelUnsupported pins the capability: the Amit adapter has no native
// cancel seam (keystrokes are forbidden), so CancelExact is
// CancelUnsupported for a live run and noop_already_terminal for a finished
// one — never a fabricated confirmed cancellation.
func TestCancelUnsupported(t *testing.T) {
	src := &fakeSource{}
	a := newTestAttachment(t, src)
	a.deliverDir = t.TempDir()
	key := testKey("r5")
	src.add(ExtEntry{Kind: "user_message", ClientRef: clientRef(key), Text: "q"})
	if _, err := a.Submit(core.BoundRequest{Key: key, Epoch: "ep1", Input: protocol.SubmitInput{Text: "q"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ev, err := a.CancelExact(key, "ep1")
	if err != nil {
		t.Fatalf("CancelExact: %v", err)
	}
	if ev.Disposition != protocol.CancelUnsupported {
		t.Fatalf("cancel disposition = %q, want unsupported (no native cancel seam)", ev.Disposition)
	}
}

// TestInspectProjectsAmitCapabilities pins the Inspect projection: harness
// amit, submit true, steer FALSE in v1 (ADR: deliver=steer disabled; the
// extension refuses with a refused status line), cancel/approve/question false, terminal
// unavailable, evidence submit=submitted.
func TestInspectProjectsAmitCapabilities(t *testing.T) {
	src := &fakeSource{}
	a := newTestAttachment(t, src)
	s := a.Inspect()
	if s.Harness != "amit" || s.TargetID != "amit" || s.Epoch != "ep1" {
		t.Fatalf("session = %+v, want amit/ep1 projection", s)
	}
	c := s.Capabilities
	if !c.Inspect || !c.Submit {
		t.Fatalf("capabilities = %+v, want inspect+submit true", c)
	}
	if c.Steer {
		t.Fatalf("capabilities = %+v, want Steer false in v1 (deliver=steer disabled)", c)
	}
	if c.CancelRequest || c.ApproveTool || c.AnswerQuestion {
		t.Fatalf("capabilities = %+v, want cancel/approve/question false", c)
	}
	if c.Terminal != "unavailable" {
		t.Fatalf("terminal = %q, want unavailable", c.Terminal)
	}
	if s.Evidence == nil || s.Evidence.Submit != "submitted" {
		t.Fatalf("evidence = %+v, want submit=submitted (never admitted)", s.Evidence)
	}
}

// TestSteerDeliversThroughInbox pins steer: a deliver=steer submit goes
// through the same inbox with deliver_as=steer (pi's deliverAs follow-up vs
// steer), admitted by the same user_message confirmation.
// TestSteerRefusedStatusLineRejectsNeverConfirms pins the v1 steer contract
// (ADR: deliver=steer disabled): the extension answers the inbox request with
// {kind:"status",client_ref,status:"refused",...}; that positive refusal must
// record the run terminal-rejected (EventRunFailed) — never confirmed by a
// later user_message, never left uncertain forever.
func TestSteerRefusedStatusLineRejectsNeverConfirms(t *testing.T) {
	src := &fakeSource{}
	a := newTestAttachment(t, src)
	a.deliverDir = t.TempDir()
	key := testKey("r6")
	// Real-world order: Submit delivers to the inbox (run bound, uncertain —
	// pi's send primitive gives no evidence), the extension then reads the
	// request and refuses it, and the refusal line lands in the log where a
	// later consume() correlates it to the bound run.
	if _, err := a.Submit(core.BoundRequest{Key: key, Epoch: "ep1", Input: protocol.SubmitInput{Text: "redirect", Deliver: protocol.DeliverSteer}}); err == nil {
		t.Fatal("Submit steer without confirming entry must return uncertain error, want non-nil")
	}
	src.add(ExtEntry{Kind: "status", ClientRef: clientRef(key), Status: "refused"})
	a.consume()
	// The run must be terminal-rejected, observable via Lookup.
	ev, err := a.Lookup(key, "ep1")
	if err != nil {
		t.Fatalf("Lookup after refusal: %v", err)
	}
	if ev.State != protocol.StateRejected {
		t.Fatalf("evidence = %+v state=%s, want rejected (refusal recorded, action required)", ev.Class, ev.State)
	}
	if ev.Class == core.EvidenceNone {
		t.Fatalf("evidence class = none, want retained evidence of the refusal")
	}
	// A late user_message carrying the same ref must NOT resurrect the
	// refused run into a confirmation.
	src.add(ExtEntry{Kind: "user_message", ClientRef: clientRef(key), Text: "redirect"})
	a.consume()
	ev, err = a.Lookup(key, "ep1")
	if err != nil {
		t.Fatalf("Lookup after late user_message: %v", err)
	}
	if ev.State != protocol.StateRejected {
		t.Fatalf("Lookup state after late user_message = %v, want still rejected", ev.State)
	}
}

// TestFactoryRejectsConfigWithoutExtension pins the factory contract: a
// manifest entry without config.extension refuses at construction.
func TestFactoryRejectsConfigWithoutExtension(t *testing.T) {
	_, err := Factory(context.Background(), registry.FactoryConfig{Target: "amit"})
	if err == nil || !strings.Contains(err.Error(), "extension is required") {
		t.Fatalf("err = %v, want extension-required refusal", err)
	}
}

// TestDeliverDuplicateRefRefuses pins the double-fire guard: delivering the
// same request ref twice refuses with ErrNotSent (create-new semantics), so
// an uncertain-record retry can never fire the prompt twice.
func TestDeliverDuplicateRefRefuses(t *testing.T) {
	a := &Attachment{now: func() time.Time { return time.Unix(0, 0) }, deliverDir: t.TempDir()}
	req := core.BoundRequest{Key: testKey("dup"), Epoch: "ep1", Input: protocol.SubmitInput{Text: "once"}}
	if err := a.deliver(req); err != nil {
		t.Fatalf("first deliver: %v", err)
	}
	err := a.deliver(req)
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("second deliver err = %v, want ErrNotSent (already delivered)", err)
	}
}

// TestEventLogSourceReadsExtensionLog pins the file seam: the JSON-line
// event log parses into entries oldest-first and the last status line wins.
func TestEventLogSourceReadsExtensionLog(t *testing.T) {
	dir := t.TempDir()
	root := dir
	ext := "amq-bridge"
	// Build the layout Dial expects.
	log := sourcePath(root, ext)
	inbox := deliverPath(root, ext)
	if err := mkdirAll(inbox); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "{\"kind\":\"status\",\"status\":\"busy\"}\n" +
		"{\"kind\":\"user_message\",\"client_ref\":\"ref-a\",\"text\":\"q\"}\n" +
		"corrupt-line\n" +
		"{\"kind\":\"status\",\"status\":\"idle\"}\n"
	if err := osWriteFile(log, content); err != nil {
		t.Fatalf("write log: %v", err)
	}
	src, err := Dial(context.Background(), registry.FactoryConfig{Root: root}, ext)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	entries := src.Entries()
	if len(entries) != 1 || entries[0].ClientRef != "ref-a" {
		t.Fatalf("entries = %+v, want the single user_message entry (corrupt line skipped)", entries)
	}
	if got := src.Status(); got != "idle" {
		t.Fatalf("status = %q, want idle (last status line wins)", got)
	}
}

// TestDialMissingLogRefuses pins the production refusal: a manifest entry
// naming an extension whose event log does not exist refuses at Dial with a
// name-the-boundary message.
func TestDialMissingLogRefuses(t *testing.T) {
	_, err := Dial(context.Background(), registry.FactoryConfig{Root: t.TempDir()}, "amq-bridge")
	if err == nil || !strings.Contains(err.Error(), "event log not found") {
		t.Fatalf("err = %v, want event-log-not-found refusal", err)
	}
}

// helpers: tiny indirections so the test file needs no extra imports.
func mkdirAll(path string) error { return os.MkdirAll(path, 0o700) }

func osWriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
