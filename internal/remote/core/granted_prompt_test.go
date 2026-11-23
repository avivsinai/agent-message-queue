package core_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// promptRuntime is the fake runtime as an attachment that gives a granted
// request's text at a prompt (core.PromptHandover). Its run events and
// Lookup carry the digest the test sets, as the Claude adapter reports what
// its transcript shows; its acknowledgements are recorded by key.
type promptRuntime struct {
	*fake.Runtime
	mu        sync.Mutex
	instance  string
	digest    string
	unknown   bool
	stored    *core.Prompt
	storedAt  time.Time
	observed  int
	forgets   int
	acked     []requests.Key
	sink      func(core.NativeEvent)
	onObserve func()
}

func newPromptRuntime() *promptRuntime {
	return &promptRuntime{Runtime: fake.New("fake", "e_1"), instance: "inst-1"}
}

func (r *promptRuntime) PromptInstance() string { return r.instance }

func (r *promptRuntime) GrantedStubID(k requests.Key) string { return "stub-" + k.RequestID }

func (r *promptRuntime) PromptAsks(p core.Prompt) (bool, string) {
	asks := p.Mode == "default" || p.Mode == "acceptEdits" || p.Mode == "dontAsk"
	return asks && p.NativeSession == r.NativeSessionID(), p.Mode
}

func (r *promptRuntime) ObservePrompt(p core.Prompt, at time.Time) {
	r.mu.Lock()
	r.observed++
	r.stored, r.storedAt = &p, at
	hook := r.onObserve
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (r *promptRuntime) ForgetPrompt(before time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgets++
	if r.stored != nil && r.storedAt.Before(before) {
		r.stored = nil
	}
}

func (r *promptRuntime) Subscribe(fn func(core.NativeEvent)) func() {
	r.mu.Lock()
	r.sink = fn
	r.mu.Unlock()
	return r.Runtime.Subscribe(func(ev core.NativeEvent) {
		switch ev.Type {
		case core.EventRunCompleted, core.EventRunFailed, core.EventRunCancelled:
			r.mu.Lock()
			ev.GrantedTextDigest = r.digest
			r.mu.Unlock()
		}
		fn(ev)
	})
}

func (r *promptRuntime) Lookup(k requests.Key, epoch string) (core.Evidence, error) {
	r.mu.Lock()
	unknown, digest := r.unknown, r.digest
	r.mu.Unlock()
	if unknown {
		return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
	}
	ev, err := r.Runtime.Lookup(k, epoch)
	ev.GrantedTextDigest = digest
	return ev, err
}

func (r *promptRuntime) AcknowledgeResult(k requests.Key, epoch, digest string) {
	r.mu.Lock()
	r.acked = append(r.acked, k)
	r.mu.Unlock()
	r.Runtime.AcknowledgeResult(k, epoch, digest)
}

func (r *promptRuntime) set(f func(r *promptRuntime)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

// emit sends ev to the endpoint as the attachment's own event.
func (r *promptRuntime) emit(ev core.NativeEvent) {
	r.mu.Lock()
	sink := r.sink
	r.mu.Unlock()
	sink(ev)
}

const grantText = "run the tests"

func sha(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// grantFixture is a link endpoint over a store in a known directory, with a
// grant check whose answer the test sets.
type grantFixture struct {
	ep     *core.Endpoint
	store  *requests.Store
	clk    *linkClock
	dir    string
	mu     sync.Mutex
	reason string
	// crash, when set before open, is the endpoint's crash hook.
	crash core.CrashPoint
}

func newGrantFixture(t *testing.T, att core.Attachment) *grantFixture {
	t.Helper()
	f := &grantFixture{clk: &linkClock{t: time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)}, dir: t.TempDir()}
	f.open(t, att)
	return f
}

// open builds an endpoint over the fixture's store directory, as a restart
// does, with att registered when it is not nil.
func (f *grantFixture) open(t *testing.T, att core.Attachment) {
	t.Helper()
	if f.ep != nil {
		_ = f.ep.Close()
	}
	store, err := requests.Open(f.dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ep := core.New(core.Config{Store: store, Now: f.clk.now, Consent: func(string, string) bool { return true }, Crash: f.crash})
	ep.SetGrant(func(string, string, string) string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.reason
	})
	if att != nil {
		ep.Register(att)
	}
	t.Cleanup(func() { _ = ep.Close() })
	f.ep, f.store = ep, store
}

func (f *grantFixture) revoke(reason string) {
	f.mu.Lock()
	f.reason = reason
	f.mu.Unlock()
}

func (f *grantFixture) submit(t *testing.T, id string, granted bool) protocol.Reply {
	t.Helper()
	src := linkSource(hostA, "fake")
	if granted {
		src.GrantDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000003"
	}
	return submitReply(t, f.ep, linkSubmit(f.clk, "fake", id), src)
}

func (f *grantFixture) record(t *testing.T, id string) *requests.Record {
	t.Helper()
	rec, ok, err := f.store.Get(requests.Key{CreatorHost: hostA, TargetID: "fake", RequestID: id})
	if err != nil || !ok {
		t.Fatalf("record %s: %v %v", id, ok, err)
	}
	return rec
}

func (f *grantFixture) get(t *testing.T, id string) protocol.Reply {
	t.Helper()
	out, err := f.ep.Handle(getCmd(protocol.EncodeRef(hostA, "fake", id)), linkSource(hostA, "fake"))
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return out.(protocol.Reply)
}

func report(id, mode string) core.PromptReport {
	return core.PromptReport{TargetID: "fake", Instance: "inst-1", Prompt: core.Prompt{NativeSession: "fake", Mode: mode}, StubID: "stub-" + id}
}

// readOnlyRecords makes the records of hostA unwritable until the returned
// function runs, so a commit fails.
func (f *grantFixture) readOnlyRecords(t *testing.T) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(f.dir, "v1", "requests", hostA)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	restore := func() { _ = os.Chmod(dir, 0o700) }
	t.Cleanup(restore)
	return restore
}

// Session grant, Claude Code: the text of a request dispatched as a stub
// leaves the endpoint only at GrantedPrompt, once, after every check passed
// with the values of that call.
func TestGrantedPromptHandsTheTextOverOnce(t *testing.T) {
	t.Run("the asking prompt gets the text once and the record holds the mark", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111201"
		f.submit(t, id, true)
		if rec := f.record(t, id); !rec.GrantAtPrompt || rec.State != protocol.StateRunning {
			t.Fatalf("granted dispatch: grant_at_prompt %v state %s; want the mark and running", rec.GrantAtPrompt, rec.State)
		}
		if got := f.ep.GrantedPrompt(report(id, "default")); got != grantText {
			t.Fatalf("first prompt = %q, want the text", got)
		}
		if !f.record(t, id).GrantTextGiven {
			t.Fatal("the record does not hold grant_text_given after the hand-over")
		}
		if got := f.ep.GrantedPrompt(report(id, "default")); got != "" {
			t.Fatalf("second prompt for the same request = %q, want no text", got)
		}
	})

	refusals := []struct {
		name   string
		before func(f *grantFixture, rt *promptRuntime)
		report func(id string) core.PromptReport
		code   protocol.Code
		reason string
	}{
		{name: "another session", report: func(id string) core.PromptReport {
			r := report(id, "default")
			r.Prompt.NativeSession = "other"
			return r
		}, code: protocol.CodeSessionChanged},
		{name: "not_after passed", before: func(f *grantFixture, _ *promptRuntime) { f.clk.advance(3 * time.Minute) }, code: protocol.CodeExpired},
		{name: "epoch changed", before: func(_ *grantFixture, rt *promptRuntime) { rt.SwitchSession("e_2") }, code: protocol.CodeStaleEpoch},
		{name: "grant revoked before the call", before: func(f *grantFixture, _ *promptRuntime) { f.revoke("grant_revoked: the owner revoked the grant") },
			code: protocol.CodeConsentInvalid, reason: "grant_revoked: the owner revoked the grant"},
		// The stored report of an earlier prompt asks; the value of this call
		// decides.
		{name: "this prompt's mode does not ask", before: func(f *grantFixture, _ *promptRuntime) {
			r := report("", "default")
			r.StubID = ""
			f.ep.GrantedPrompt(r)
		}, report: func(id string) core.PromptReport { return report(id, "plan") }, code: protocol.CodeConsentInvalid, reason: "grant_needs_signature: plan"},
	}
	for i, row := range refusals {
		t.Run(row.name, func(t *testing.T) {
			rt := newPromptRuntime()
			f := newGrantFixture(t, rt)
			id := "11111111-1111-4111-8111-11111111121" + string(rune('0'+i))
			f.submit(t, id, true)
			if row.before != nil {
				row.before(f, rt)
			}
			r := report(id, "default")
			if row.report != nil {
				r = row.report(id)
			}
			if got := f.ep.GrantedPrompt(r); got != "" {
				t.Fatalf("GrantedPrompt = %q, want no text", got)
			}
			rec := f.record(t, id)
			if rec.State != protocol.StateRejected || rec.Code != row.code || rec.GrantTextGiven {
				t.Fatalf("record = %s/%s given %v; want rejected %s, no mark", rec.State, rec.Code, rec.GrantTextGiven, row.code)
			}
			if got := f.get(t, id); got.Outcome.Message != row.reason {
				t.Fatalf("request.get message = %q, want %q", got.Outcome.Message, row.reason)
			}
		})
	}

	t.Run("another instance changes nothing", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111220"
		f.submit(t, id, true)
		r := report(id, "default")
		r.Instance = "inst-old"
		if got := f.ep.GrantedPrompt(r); got != "" || rt.observed != 0 || f.record(t, id).GrantTextGiven {
			t.Fatalf("stale locator: text %q, observed %d, mark %v; want nothing", got, rt.observed, f.record(t, id).GrantTextGiven)
		}
		// An attachment that publishes no instance (no locator) takes no
		// report, also one that names none.
		rt.set(func(r *promptRuntime) { r.instance = "" })
		r.Instance = ""
		if got := f.ep.GrantedPrompt(r); got != "" || rt.observed != 0 {
			t.Fatalf("no instance: text %q, observed %d; want nothing", got, rt.observed)
		}
	})

	t.Run("an attachment replaced between the two locks gives nothing", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111221"
		f.submit(t, id, true)
		next := newPromptRuntime()
		rt.set(func(r *promptRuntime) { r.onObserve = func() { f.ep.Register(next) } })
		if got := f.ep.GrantedPrompt(report(id, "default")); got != "" || f.record(t, id).GrantTextGiven {
			t.Fatalf("replaced attachment: text %q, mark %v; want neither", got, f.record(t, id).GrantTextGiven)
		}
	})

	t.Run("a dispatch recorded inside the wait gives the text", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111222"
		rt.HoldAdmission()
		ready := rt.NotifySubmitReady()
		done := make(chan struct{})
		go func() { defer close(done); f.submit(t, id, true) }()
		<-ready
		observed := make(chan struct{})
		rt.set(func(r *promptRuntime) { r.onObserve = func() { close(observed) } })
		r := report(id, "default")
		r.Wait = 2 * time.Second
		got := make(chan string, 1)
		go func() { got <- f.ep.GrantedPrompt(r) }()
		<-observed
		time.Sleep(20 * time.Millisecond) // let the call reach its wait
		rt.ReleaseAdmission()
		<-done
		if text := <-got; text != grantText {
			t.Fatalf("GrantedPrompt across the dispatch = %q, want the text", text)
		}
	})

	t.Run("a dispatch not recorded within the wait gives nothing and writes nothing", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111223"
		rt.HoldAdmission()
		ready := rt.NotifySubmitReady()
		done := make(chan struct{})
		go func() { defer close(done); f.submit(t, id, true) }()
		<-ready
		before := f.record(t, id).Revision
		r := report(id, "default")
		r.Wait = 30 * time.Millisecond
		if got := f.ep.GrantedPrompt(r); got != "" {
			t.Fatalf("GrantedPrompt while dispatching = %q, want no text", got)
		}
		if rec := f.record(t, id); rec.Revision != before || rec.GrantTextGiven || rec.State != protocol.StateDispatching {
			t.Fatalf("record after the wait = rev %d (was %d) %s mark %v; want unchanged", rec.Revision, before, rec.State, rec.GrantTextGiven)
		}
		rt.ReleaseAdmission()
		<-done
	})

	t.Run("a mark that does not commit gives no text", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111224"
		f.submit(t, id, true)
		restore := f.readOnlyRecords(t)
		if got := f.ep.GrantedPrompt(report(id, "default")); got != "" {
			t.Fatalf("GrantedPrompt with a failed write = %q, want no text", got)
		}
		restore()
		if f.record(t, id).GrantTextGiven {
			t.Fatal("the mark is on the record after its write failed")
		}
	})
}

// Session grant, Claude Code: core settles a request dispatched as a stub
// from the record alone. It completes only when the evidence ties the answer
// to the text it gave; an answer it cannot tie is withheld for good; a stub
// nobody took ends at its deadline.
func TestGrantedStubSettlement(t *testing.T) {
	t.Run("the right digest after the hand-over completes with the answer", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111301"
		f.submit(t, id, true)
		f.ep.GrantedPrompt(report(id, "default"))
		rt.set(func(r *promptRuntime) { r.digest = sha(grantText) })
		rt.Complete(id, "all green")
		rec := f.record(t, id)
		if rec.State != protocol.StateCompleted || rec.Result == nil || rec.Result.Text != "all green" || rt.forgets != 0 {
			t.Fatalf("record = %s result %+v forgets %d; want completed with the answer, the report kept", rec.State, rec.Result, rt.forgets)
		}
	})

	unattributed := []struct {
		name   string
		give   bool
		digest string
	}{
		{name: "another digest", give: true, digest: sha("another text")},
		{name: "no digest", give: true},
		{name: "no hand-over", digest: sha(grantText)},
	}
	for i, row := range unattributed {
		t.Run(row.name, func(t *testing.T) {
			rt := newPromptRuntime()
			f := newGrantFixture(t, rt)
			id := "11111111-1111-4111-8111-11111111131" + string(rune('0'+i))
			key := requests.Key{CreatorHost: hostA, TargetID: "fake", RequestID: id}
			f.submit(t, id, true)
			if row.give {
				f.ep.GrantedPrompt(report(id, "default"))
			}
			rt.set(func(r *promptRuntime) { r.digest = row.digest })
			rt.Complete(id, "the answer")
			withheld := func(when string) {
				t.Helper()
				rec := f.record(t, id)
				if rec.State != protocol.StateFailed || rec.Result == nil || rec.Result.Text != "" {
					t.Fatalf("%s: record = %s result %+v; want failed with no text", when, rec.State, rec.Result)
				}
				if got := f.get(t, id); got.Outcome.Message != "the answer cannot be attributed to this task; the task may have run" {
					t.Fatalf("%s: request.get message = %q", when, got.Outcome.Message)
				}
			}
			withheld("at the end")
			rt.set(func(r *promptRuntime) {
				if len(r.acked) != 1 || r.acked[0] != key || r.forgets != 1 {
					t.Fatalf("acks %v forgets %d; want the key acknowledged and the report forgotten once", r.acked, r.forgets)
				}
			})
			revision := f.record(t, id).Revision
			for _, late := range []core.NativeEvent{
				{Type: core.EventRunCompleted, Key: key, RunID: "run_1", Result: &protocol.Result{Text: "the answer"}, GrantedTextDigest: sha(grantText)},
				{Type: core.EventRunCompleted, Key: key, RunID: "run_1", Result: &protocol.Result{Text: "another answer"}, GrantedTextDigest: sha(grantText)},
				{Type: core.EventRunCancelled, Key: key, RunID: "run_1", Result: &protocol.Result{Text: "partial"}, GrantedTextDigest: sha(grantText)},
			} {
				rt.emit(late)
				withheld("after a late " + string(late.Type) + " " + late.Result.Text)
			}
			// A late event stores what the record already holds, so it is a
			// duplicate and writes no new revision.
			if got := f.record(t, id).Revision; got != revision {
				t.Fatalf("late events moved the revision %d -> %d", revision, got)
			}
			f.open(t, rt)
			if err := f.ep.Reconcile(); err != nil {
				t.Fatalf("reconcile after a restart: %v", err)
			}
			withheld("after a restart")
		})
	}

	// The acknowledgement of a withheld answer is replayed after a crash
	// between its intent and the call: the adapter's retained result is
	// compared in the form the record stores it.
	t.Run("a withheld answer's acknowledgement is replayed after a crash", func(t *testing.T) {
		rt := newPromptRuntime()
		f := &grantFixture{clk: &linkClock{t: time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)}, dir: t.TempDir()}
		f.crash = func(point string) error {
			if point == core.PointBeforeAck {
				return os.ErrDeadlineExceeded
			}
			return nil
		}
		f.open(t, rt)
		const id = "11111111-1111-4111-8111-111111111319"
		f.submit(t, id, true)
		rt.Complete(id, "the answer")
		if rec := f.record(t, id); rec.State != protocol.StateFailed || rec.AckDigest == "" || len(rt.acked) != 0 {
			t.Fatalf("before the replay: %s ack intent %q acks %d; want failed with the intent and no ack", rec.State, rec.AckDigest, len(rt.acked))
		}
		f.crash = nil
		f.open(t, rt)
		if err := f.ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rec := f.record(t, id); len(rt.acked) != 1 || !rec.Acknowledged {
			t.Fatalf("after the replay: acks %d acknowledged %v; want one ack, recorded", len(rt.acked), rec.Acknowledged)
		}
	})

	// A restart reads the run's end from Lookup, not from an event.
	t.Run("a run that ended while the endpoint was down is judged at the restart", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111317"
		f.submit(t, id, true)
		f.ep.GrantedPrompt(report(id, "default"))
		f.open(t, nil)
		rt.set(func(r *promptRuntime) { r.digest = sha("another text") })
		rt.Complete(id, "the answer")
		if rec := f.record(t, id); rec.State != protocol.StateRunning {
			t.Fatalf("record while the endpoint was down = %s, want running", rec.State)
		}
		f.open(t, rt)
		if err := f.ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rec := f.record(t, id); rec.State != protocol.StateFailed || rec.Result == nil || rec.Result.Text != "" || rt.forgets != 1 {
			t.Fatalf("record = %s result %+v forgets %d; want failed with no text and the report forgotten", rec.State, rec.Result, rt.forgets)
		}
		if got := f.get(t, id); got.Outcome.Message != "the answer cannot be attributed to this task; the task may have run" {
			t.Fatalf("request.get message = %q", got.Outcome.Message)
		}
	})

	// Pre-merge review: the Submit that returns after the run ended failed
	// again with no reason and cleared the withheld answer's reason.
	t.Run("a run that ends before its Submit returns keeps the withheld reason", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111318"
		rt.HoldAfterAdmit()
		done := make(chan struct{})
		go func() { defer close(done); f.submit(t, id, true) }()
		for deadline := time.Now().Add(2 * time.Second); !rt.Complete(id, "the answer"); {
			if time.Now().After(deadline) {
				t.Fatal("the run was never admitted")
			}
			time.Sleep(time.Millisecond)
		}
		rt.ReleaseAfterAdmit()
		<-done
		if rec := f.record(t, id); rec.State != protocol.StateFailed || (rec.Result != nil && rec.Result.Text != "") {
			t.Fatalf("record = %s result %+v; want failed with no text", rec.State, rec.Result)
		}
		if got := f.get(t, id); got.Outcome.Message != "the answer cannot be attributed to this task; the task may have run" {
			t.Fatalf("request.get message = %q", got.Outcome.Message)
		}
	})

	t.Run("a late answer on a request that expired untaken stores no text", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111320"
		f.submit(t, id, true)
		f.clk.advance(3 * time.Minute)
		if err := f.ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		rt.set(func(r *promptRuntime) { r.digest = sha(grantText) })
		rt.Complete(id, "the answer")
		if rec := f.record(t, id); rec.State != protocol.StateRejected || rec.Code != protocol.CodeExpired || (rec.Result != nil && rec.Result.Text != "") {
			t.Fatalf("record = %s/%s result %+v; want rejected expired with no text", rec.State, rec.Code, rec.Result)
		}
	})

	t.Run("a stub nobody took ends at its deadline, also after a failed write", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111321"
		f.submit(t, id, true)
		f.clk.advance(3 * time.Minute)
		restore := f.readOnlyRecords(t)
		if err := f.ep.Reconcile(); err == nil {
			t.Fatal("reconcile with unwritable records reported no error")
		}
		restore()
		if err := f.ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		rec := f.record(t, id)
		if rec.State != protocol.StateRejected || rec.Code != protocol.CodeExpired || rt.forgets != 1 {
			t.Fatalf("record = %s/%s forgets %d; want rejected expired and the report forgotten", rec.State, rec.Code, rt.forgets)
		}
		if got := f.get(t, id); got.Outcome.Message != "the session did not take the task before its deadline" {
			t.Fatalf("request.get message = %q", got.Outcome.Message)
		}
	})

	t.Run("a restart with no attachment: uncertain, then expired at the deadline", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111322"
		f.submit(t, id, true)
		f.open(t, nil)
		if err := f.ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rec := f.record(t, id); rec.State != protocol.StateUncertain {
			t.Fatalf("after a restart with no attachment = %s, want uncertain", rec.State)
		}
		f.clk.advance(3 * time.Minute)
		if err := f.ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rec := f.record(t, id); rec.State != protocol.StateRejected || rec.Code != protocol.CodeExpired {
			t.Fatalf("at the deadline with no attachment = %s/%s, want rejected expired", rec.State, rec.Code)
		}
	})

	t.Run("a given text with unknown evidence past the deadline stays uncertain", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const id = "11111111-1111-4111-8111-111111111323"
		f.submit(t, id, true)
		f.ep.GrantedPrompt(report(id, "default"))
		rt.set(func(r *promptRuntime) { r.unknown = true })
		f.clk.advance(3 * time.Minute)
		if err := f.ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rec := f.record(t, id); rec.State != protocol.StateUncertain {
			t.Fatalf("record = %s, want uncertain", rec.State)
		}
	})

	t.Run("records not dispatched as a stub take today's paths", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const signed = "11111111-1111-4111-8111-111111111324"
		cmd := linkSubmit(f.clk, "fake", signed)
		cmd.Input.Text = "AMQ granted task " + signed
		submitReply(t, f.ep, cmd, linkSource(hostA, "fake"))
		rt.Complete(signed, "signed answer")
		if rec := f.record(t, signed); rec.GrantAtPrompt || rec.State != protocol.StateCompleted || rec.Result.Text != "signed answer" {
			t.Fatalf("signed record = mark %v %s %+v; want completed with its answer", rec.GrantAtPrompt, rec.State, rec.Result)
		}

		plain := fake.New("fake", "e_1")
		g := newGrantFixture(t, plain)
		const granted = "11111111-1111-4111-8111-111111111325"
		g.submit(t, granted, true)
		plain.Complete(granted, "granted answer")
		if rec := g.record(t, granted); rec.GrantAtPrompt || rec.State != protocol.StateCompleted || rec.Result.Text != "granted answer" {
			t.Fatalf("granted record of an adapter that sends text = mark %v %s %+v; want completed with its answer", rec.GrantAtPrompt, rec.State, rec.Result)
		}
	})
}
