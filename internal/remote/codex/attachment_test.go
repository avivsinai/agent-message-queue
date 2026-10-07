package codex

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// fakeAppServer answers the app-server methods the attachment uses with the
// shapes recorded from the live probe and the generated schema, and lets the
// test push notifications.
type fakeAppServer struct {
	ws                  *wsConn
	calls               chan rpcMessage
	threadReadHandler   func() string
	threadReadHandlerMu sync.Mutex
	turnStartDelayHook  func()
	turnStartDelayMu    sync.Mutex
	turnStartResponse   string
	turnStartResponseMu sync.Mutex
	resumeReviewer      string
	resumeReviewerMu    sync.Mutex
}

// waitMemoForID waits until the terminal memo records id (deterministic sync
// point replacing sleep-based assertions; 7xl agent-message-queue-7xl).
func waitMemoForID(t *testing.T, att *Attachment, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		att.mu.Lock()
		ok := att.terminalTurns[id]
		att.mu.Unlock()
		if ok {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("terminal memo never recorded %s", id)
}

// waitLostStateGen waits until the read pump has processed a lost-state
// notification (the gen bump is the fingerprint). It is a deterministic
// sync point: srv.notify writes the frame, the read pump applies it
// asynchronously, and the gen counter is the observable barrier. Replaces
// poll-sleep loops; 7xl.
func waitLostStateGen(t *testing.T, att *Attachment, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		att.mu.Lock()
		gen := att.lostStateGen
		att.mu.Unlock()
		if gen >= want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("lostStateGen never reached %d", want)
}

// waitInteraction waits until the run for key carries a registered
// interaction (deterministic sync point replacing sleep; 7xl).
func waitInteraction(t *testing.T, att *Attachment, key requests.Key) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		att.mu.Lock()
		r, ok := att.runs[key]
		set := ok && r.interaction != nil
		att.mu.Unlock()
		if set {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("interaction never registered for run")
}

func startFakeAppServer(t *testing.T) (string, *fakeAppServer) {
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	srv := &fakeAppServer{calls: make(chan rpcMessage, 16)}
	ready := make(chan struct{})
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		ws, err := acceptServerWS(conn)
		if err != nil {
			return
		}
		srv.ws = ws
		close(ready)
		for {
			payload, err := ws.readText()
			if err != nil {
				return
			}
			var msg rpcMessage
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			srv.calls <- msg
			if msg.ID == nil || msg.Method == "" {
				continue
			}
			switch msg.Method {
			case "initialize":
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{"userAgent":"fake"}}`))
			case "thread/resume":
				srv.resumeReviewerMu.Lock()
				reviewer := srv.resumeReviewer
				srv.resumeReviewerMu.Unlock()
				if reviewer == "" {
					reviewer = "user"
				}
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{"thread":{"id":"t1","cwd":"/work","status":{"type":"idle"}},"approvalsReviewer":"` + reviewer + `"}}`))
			case "turn/start":
				srv.turnStartDelayMu.Lock()
				hook := srv.turnStartDelayHook
				srv.turnStartDelayMu.Unlock()
				if hook != nil {
					hook()
				}
				srv.turnStartResponseMu.Lock()
				customResp := srv.turnStartResponse
				srv.turnStartResponseMu.Unlock()
				if customResp != "" {
					_ = ws.writeText([]byte(fmt.Sprintf(customResp, string(*msg.ID))))
				} else {
					_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{"turn":{"id":"u1","status":"inProgress"}}}`))
				}
			case "turn/interrupt":
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{}}`))
			case "thread/queue/add":
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{}}`))
			case "thread/read":
				srv.threadReadHandlerMu.Lock()
				handler := srv.threadReadHandler
				srv.threadReadHandlerMu.Unlock()
				if handler != nil {
					_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":` + handler() + `}`))
				} else {
					_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"error":{"code":-32601,"message":"no thread/read handler"}}`))
				}
			default:
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"error":{"code":-32601,"message":"unexpected ` + msg.Method + `"}}`))
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-ready:
		default:
		}
	})
	return sock, srv
}

func (s *fakeAppServer) notify(t *testing.T, method, params string) {
	if err := s.ws.writeText([]byte(`{"jsonrpc":"2.0","method":"` + method + `","params":` + params + `}`)); err != nil {
		t.Fatalf("notify %s: %v", method, err)
	}
}

// sendServerRequest sends a JSON-RPC request (with ID and method) from the
// fake server to the client. Used to simulate approval requests.
func (s *fakeAppServer) sendServerRequest(t *testing.T, id, method, params string) {
	if err := s.ws.writeText([]byte(`{"jsonrpc":"2.0","id":"` + id + `","method":"` + method + `","params":` + params + `}`)); err != nil {
		t.Fatalf("sendServerRequest %s: %v", method, err)
	}
}

// setThreadReadHandler installs a custom handler for thread/read responses.
// The handler returns the JSON result string for the thread/read call.
func (s *fakeAppServer) setThreadReadHandler(fn func() string) {
	s.threadReadHandlerMu.Lock()
	defer s.threadReadHandlerMu.Unlock()
	s.threadReadHandler = fn
}

// setResumeReviewer sets the approvalsReviewer thread/resume answers with
// (default "user").
func (s *fakeAppServer) setResumeReviewer(v string) {
	s.resumeReviewerMu.Lock()
	defer s.resumeReviewerMu.Unlock()
	s.resumeReviewer = v
}

// setTurnStartDelay installs a hook that fires BEFORE the turn/start RPC
// response is written. Used by B3 to simulate the race where the read pump
// processes notifications before the Submit goroutine processes the RPC
// response.
func (s *fakeAppServer) setTurnStartDelay(fn func()) {
	s.turnStartDelayMu.Lock()
	defer s.turnStartDelayMu.Unlock()
	s.turnStartDelayHook = fn
}

// setTurnStartResponse overrides the default turn/start RPC response. The
// format string must contain %s where the RPC id goes. Used by B1 to send a
// malformed response that fails to decode.
func (s *fakeAppServer) setTurnStartResponse(format string) {
	s.turnStartResponseMu.Lock()
	defer s.turnStartResponseMu.Unlock()
	s.turnStartResponse = format
}

// TestSubmitBindsTurnAndCompletes is the adapter happy path: submit starts a
// turn with our request id as clientUserMessageId, the user-message item
// echoes it back, the agent message carries the text, and turn/completed
// yields a completed run with that text.
func TestSubmitBindsTurnAndCompletes(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	// drain initialize + thread/resume
	<-srv.calls
	<-srv.calls

	events := make(chan core.NativeEvent, 8)
	att.Subscribe(func(ev core.NativeEvent) { events <- ev })

	s := att.Inspect()
	if s.Harness != "codex" || s.Status != "idle" || s.Project != "/work" || !s.Capabilities.Submit || s.Capabilities.ApproveTool {
		t.Fatalf("unexpected session: %+v", s)
	}

	// Submit blocks until our userMessage item confirms the turn accepted our
	// text, so run it concurrently and deliver the confirming notification.
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111501"}
	type admResult struct {
		adm core.Admission
		err error
	}
	done := make(chan admResult, 1)
	go func() {
		adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		done <- admResult{adm, err}
	}()
	call := <-srv.calls
	var params map[string]any
	_ = json.Unmarshal(call.Params, &params)
	if call.Method != "turn/start" || params["clientUserMessageId"] != clientIDFor(key) {
		t.Fatalf("unexpected native call: %s %v", call.Method, params)
	}
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	select {
	case r := <-done:
		if r.err != nil || !r.adm.Admitted || r.adm.RunID != "turn:u1" {
			t.Fatalf("submit: %+v %v", r.adm, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("submit did not admit after userMessage confirmation")
	}

	srv.notify(t, "item/completed", `{"threadId":"t1","turnId":"u1","completedAtMs":1,"item":{"type":"agentMessage","id":"i2","text":"PONG"}}`)
	// A second submit while the turn is active is refused as busy, never
	// joined to the running turn.
	adm2, _ := att.Submit(core.BoundRequest{Key: requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111502"}, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "again"}})
	if adm2.Admitted || adm2.Code != protocol.CodeBusy {
		t.Fatalf("busy submit not refused: %+v", adm2)
	}
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"u1","status":"completed"}}`)

	select {
	case ev := <-events:
		if ev.Type != core.EventRunCompleted || ev.Key != key || ev.Result == nil || ev.Result.Text != "PONG" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no completion event")
	}
	ev, err := att.Lookup(key, s.Epoch)
	if err != nil || !ev.Known || ev.State != protocol.StateCompleted || ev.Result.Text != "PONG" {
		t.Fatalf("lookup: %+v %v", ev, err)
	}
	if att.Inspect().Status != "idle" {
		t.Fatal("thread not idle after completion")
	}
}

// TestTentativeRunIsNotOwned reproduces Pro finding B01: while a run is bound
// but not yet confirmed by its own userMessage item, no consumer may treat it
// as owned. Lookup must report Tentative (not Admitted), CancelExact must
// record intent without interrupting a turn we do not own, a foreign turn's
// completion must not complete our run, and once our userMessage confirms the
// run the pending cancel is delivered against OUR turn.
func TestTentativeRunIsNotOwned(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	key := requests.Key{CreatorHost: "local", TargetID: att.Inspect().TargetID, RequestID: "11111111-1111-4111-8111-111111111701"}
	epoch := att.Inspect().Epoch
	done := make(chan core.Admission, 1)
	go func() {
		adm, _ := att.Submit(core.BoundRequest{Key: key, Epoch: epoch, Input: protocol.SubmitInput{Text: "do it"}})
		done <- adm
	}()
	call := <-srv.calls // turn/start; server replies turn id "u1"
	if call.Method != "turn/start" {
		t.Fatalf("expected turn/start, got %s", call.Method)
	}

	// Tentative window: the run is bound (byClientID) but not confirmed.
	ev, err := att.Lookup(key, epoch)
	if err != nil || ev.Class != core.EvidenceTentative || ev.Admitted {
		t.Fatalf("tentative Lookup wrong: class=%s admitted=%v err=%v", ev.Class, ev.Admitted, err)
	}
	ce, _ := att.CancelExact(key, epoch)
	if ce.Disposition != protocol.CancelRequested {
		t.Fatalf("tentative cancel disposition=%s, want cancel_requested", ce.Disposition)
	}
	// A foreign turn completing must not complete our run.
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"uForeign","status":"completed"}}`)
	// 7xl: was a 30ms sleep (negative assertion, proves nothing and fails
	// under load). Deterministic sync point: wait until the pump has actually
	// processed the foreign completion (memo records it unconditionally,
	// agent-message-queue-611.22.36 10a), then assert our run was untouched.
	waitMemoForID(t, att, "uForeign")
	if lk, _ := att.Lookup(key, epoch); lk.State == protocol.StateCompleted {
		t.Fatal("foreign turn/completed wrongly completed our run")
	}
	// No turn/interrupt may have been sent yet (we never owned a turn).
	select {
	case c := <-srv.calls:
		if c.Method == "turn/interrupt" {
			t.Fatal("interrupt sent for an unconfirmed run")
		}
	default:
	}

	// Confirm: our userMessage lands for turn u1. This binds byTurn and must
	// deliver the pending cancel as an interrupt for OUR turn u1.
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	select {
	case adm := <-done:
		if !adm.Admitted {
			t.Fatalf("submit did not admit after confirmation: %+v", adm)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("submit never returned after confirmation")
	}
	// The pending cancel now fires turn/interrupt for u1.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case c := <-srv.calls:
			if c.Method == "turn/interrupt" {
				var p map[string]any
				_ = json.Unmarshal(c.Params, &p)
				if p["turnId"] != "u1" {
					t.Fatalf("interrupt hit turn %v, want our turn u1", p["turnId"])
				}
				return
			}
		case <-deadline:
			t.Fatal("pending cancel never delivered an interrupt for our turn")
		}
	}
}

// TestTargetIDNoCollision reproduces Pro B03: two distinct thread ids that
// share a 12-hex prefix must not collapse to one target id.
func TestTargetIDNoCollision(t *testing.T) {
	a := TargetID("0123456789ab-cdef-0000-0000-000000000001")
	b := TargetID("0123456789ab-cdef-0000-0000-000000000002")
	if a == b {
		t.Fatalf("distinct threads collapsed to one target: %s", a)
	}
}

// TestAcknowledgeResultReleasesRetainedEvidence reproduces
// agent-message-queue-611.22.24 (codex ack no-op): AcknowledgeResult was an
// empty function and nothing was ever removed, so the endpoint's convergence
// precondition — "once a native ack lands the attachment retains nothing and
// Lookup reports EvidenceNone" — never held for codex. Every terminal record
// re-asked Lookup on every reconcile tick, forever, and each miss cost a full
// thread/read of the transcript.
func TestAcknowledgeResultReleasesRetainedEvidence(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111801"}
	done := make(chan struct{})
	go func() {
		_, _ = att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "work"}})
		close(done)
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	<-done
	srv.notify(t, "item/completed", `{"threadId":"t1","turnId":"u1","completedAtMs":1,"item":{"type":"agentMessage","id":"i2","text":"OUT"}}`)
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"u1","status":"completed"}}`)

	// 7xl: was a 10ms-sleep poll loop. Deterministic: wait for the terminal
	// observation to reach the memo, then a single Lookup must be terminal.
	waitMemoForID(t, att, "u1")
	ev, _ := att.Lookup(key, s.Epoch)
	if ev.State != protocol.StateCompleted || ev.Result == nil || ev.Result.Text != "OUT" {
		t.Fatalf("terminal evidence not retained before ack: %+v", ev)
	}

	// A wrong digest must not release a different request's result.
	att.AcknowledgeResult(key, s.Epoch, protocol.EvidenceDigest(&protocol.Result{Text: "something else"}))
	if lk, _ := att.Lookup(key, s.Epoch); lk.Class == core.EvidenceNone {
		t.Fatal("a wrong-digest ack released the retained evidence")
	}

	// The matching digest releases it, and Lookup must report that nothing is
	// retained WITHOUT falling through to the thread/read history (whose copy
	// would look like fresh evidence and restart the ack loop).
	att.AcknowledgeResult(key, s.Epoch, protocol.EvidenceDigest(ev.Result))
	drained := len(srv.calls)
	lk, err := att.Lookup(key, s.Epoch)
	if err != nil {
		t.Fatalf("lookup after ack: %v", err)
	}
	if lk.Class != core.EvidenceNone || lk.Result != nil {
		t.Fatalf("ack did not release retained evidence: class=%s result=%+v", lk.Class, lk.Result)
	}
	if len(srv.calls) != drained {
		t.Fatal("Lookup after ack fell through to a thread/read history call")
	}
}

// TestUnconfirmedTurnStartIsUncertainNotRejected reproduces
// agent-message-queue-611.22.31 (codex unconfirmed -> rejected): turn/start
// had already returned a turn id, so the text may be RUNNING in Codex, but an
// unconfirmed start returned a REFUSAL code. The endpoint committed a
// terminal `rejected` record it never reconciles again; the caller retried
// with a fresh id and the prompt ran twice. Absence of our confirmation is
// uncertainty, not proof of refusal: report an error (the endpoint's uncertain
// path) and keep the correlation so the run can still be resolved.
func TestUnconfirmedTurnStartIsUncertainNotRejected(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111802"}
	type admResult struct {
		adm core.Admission
		err error
	}
	done := make(chan admResult, 1)
	go func() {
		adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "work"}})
		done <- admResult{adm, err}
	}()
	<-srv.calls // turn/start; the server answers with a turn id
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	// No userMessage item ever confirms the turn. Close the app-server so the
	// client.Done() arm fires instead of waiting out the confirm timeout.
	_ = srv.ws.close()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("unconfirmed turn/start returned no error: %+v", r.adm)
		}
		if r.adm.Code != "" {
			t.Fatalf("unconfirmed turn/start returned refusal code %q; it must be uncertain, not a refusal", r.adm.Code)
		}
		if r.adm.RunID == "" {
			t.Fatal("unconfirmed turn/start dropped the run id; the correlation must survive so the run can be resolved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submit did not return after the app-server closed")
	}
}

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// TestLargeResultReleasedByBoundedDigest reproduces Pro F2: the attachment
// digested the UNBOUNDED result while the endpoint digested the BOUNDED one,
// so any result larger than MaxResultBytes was never released. The fix: the
// attachment bounds the result at the source (r.result() returns the bounded
// form), so both sides digest the same bytes.
func TestLargeResultReleasedByBoundedDigest(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111811"}
	done := make(chan struct{})
	go func() {
		_, _ = att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "work"}})
		close(done)
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	<-done
	// Emit a result LARGER than MaxResultBytes. The attachment must bound it
	// before digesting, so the endpoint's bounded digest matches.
	bigText := strings.Repeat("x", protocol.MaxResultBytes+50_000)
	srv.notify(t, "item/completed", `{"threadId":"t1","turnId":"u1","completedAtMs":1,"item":{"type":"agentMessage","id":"i2","text":"`+bigText+`"}}`)
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"u1","status":"completed"}}`)

	// 7xl: was a 10ms-sleep poll loop (same shape as above).
	waitMemoForID(t, att, "u1")
	ev, _ := att.Lookup(key, s.Epoch)
	if ev.State != protocol.StateCompleted {
		t.Fatalf("terminal evidence not retained: %+v", ev)
	}
	if ev.Result == nil || !ev.Result.Truncated {
		t.Fatalf("result not bounded: truncated=%v (want true, result > MaxResultBytes)", ev.Result != nil && ev.Result.Truncated)
	}

	// The endpoint would compute protocol.EvidenceDigest(boundResult(ev.Result)).
	// Since ev.Result is already bounded, boundResult is a no-op. The
	// attachment must accept this digest.
	att.AcknowledgeResult(key, s.Epoch, protocol.EvidenceDigest(ev.Result))
	lk, _ := att.Lookup(key, s.Epoch)
	if lk.Class != core.EvidenceNone || lk.Result != nil {
		t.Fatalf("bounded-digest ack did not release the large result — the attachment digested the unbounded form (Pro F2): class=%s", lk.Class)
	}
}

// TestHistoryResolvedRunConverges reproduces B2 (F1 never converges): when
// lookupHistory resolves a run as terminal, the in-memory run must become
// terminal so AcknowledgeResult can release it. Without the fix, the run
// stays StateRunning forever, AcknowledgeResult refuses every ack, and
// Reconcile re-reads the whole transcript every tick.
//
// It also covers Submit's confirm-timeout arm (Pro F1; the client.Done() arm
// is TestUnconfirmedTurnStartIsUncertainNotRejected): an unconfirmed
// turn/start past confirmTimeout is uncertain with its run id kept, and
// Lookup then falls through to history. The fake clock makes the timeout
// deterministic.
func TestHistoryResolvedRunConverges(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	clk := &fakeClock{t: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	att, err := Attach(sock, "t1", WithClock(clk.now), WithConfirmTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-1111111118c1"}
	type admResult struct {
		adm core.Admission
		err error
	}
	done := make(chan admResult, 1)
	go func() {
		adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "work"}})
		done <- admResult{adm, err}
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	// No confirming userMessage — advance past confirmTimeout so Lookup
	// falls through to lookupHistory.
	clk.advance(51 * time.Millisecond)
	if r := <-done; r.err == nil || r.adm.Code != "" || r.adm.RunID == "" {
		t.Fatalf("unconfirmed turn/start = %+v, %v; want uncertain (error, no code) with the run id kept", r.adm, r.err)
	}

	// Now set up the fake server to return a completed turn in thread/read
	// that carries our clientId.
	srv.setThreadReadHandler(func() string {
		return `{"thread":{"turns":[{"id":"u1","status":"completed","items":[{"type":"userMessage","clientId":"` + clientIDFor(key) + `"},{"type":"agentMessage","text":"done"}]}]}}`
	})

	// Lookup resolves the run from history as completed.
	lk, lerr := att.Lookup(key, s.Epoch)
	if lerr != nil {
		t.Fatalf("Lookup: %v", lerr)
	}
	if lk.State != protocol.StateCompleted {
		t.Fatalf("Lookup did not resolve the run as completed: state=%s", lk.State)
	}
	if lk.Result == nil || lk.Result.Text != "done" {
		t.Fatalf("Lookup did not deliver the result: %+v", lk.Result)
	}

	// B2: the in-memory run must now be terminal (completed). Without the
	// fix, it stays StateRunning and AcknowledgeResult refuses every ack.
	lk2, _ := att.Lookup(key, s.Epoch)
	if !lk2.State.Terminal() {
		t.Fatalf("in-memory run did not become terminal after history resolved it (B2): state=%s", lk2.State)
	}

	// AcknowledgeResult must now release the run (digest matches). Without
	// the fix, the run stays in a.runs because !r.state.Terminal() refuses.
	digest := protocol.EvidenceDigest(lk.Result)
	att.AcknowledgeResult(key, s.Epoch, digest)
	lk3, _ := att.Lookup(key, s.Epoch)
	if lk3.Class != core.EvidenceNone || lk3.Result != nil {
		t.Fatalf("AcknowledgeResult did not release the history-resolved run (B2): class=%s result=%+v", lk3.Class, lk3.Result)
	}
}

// TestB1PreSendFailureIsRefusalNotUncertain reproduces B1 (a)
// (agent-message-queue-611.22.35): a pre-send failure (connection already
// closed before the write) is UNAMBIGUOUS — the prompt never left. The
// adapter must dropRun + refuse (retry is safe, nothing ran). The run must
// NOT be in a.runs.
func TestB1PreSendFailureIsRefusalNotUncertain(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111b01"}

	// Make the RPC write fail before anything reaches the wire: a poisoned
	// stream (611.22.38) fails every write as pre-send while the read pump
	// stays live, so Submit reaches the turn/start error arm instead of
	// the offline refusal that a closed connection races it into.
	att.client.Load().ws.poisoned.Store(true)

	type admResult struct {
		adm core.Admission
		err error
	}
	done := make(chan admResult, 1)
	go func() {
		adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		done <- admResult{adm, err}
	}()

	select {
	case r := <-done:
		// Pre-send: refusal code, no error (the endpoint commits rejected).
		if r.err != nil {
			t.Fatalf("pre-send failure returned error %v (must be refusal, not uncertain) (B1)", r.err)
		}
		if r.adm.Code == "" {
			t.Fatal("pre-send failure returned no refusal code (B1 — the prompt never left, refusal is safe)")
		}
		// The run must NOT be in the map (dropRun was called).
		att.mu.Lock()
		_, hasRun := att.runs[key]
		att.mu.Unlock()
		if hasRun {
			t.Fatal("pre-send failure left the run in the map (B1 — dropRun must clean up)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submit did not return after pre-send failure (B1)")
	}
}

// TestB1PostSendFailureIsUncertainNotRefused reproduces B1 (b)
// (agent-message-queue-611.22.35): a post-send failure (the server
// responded but the result failed to decode) is AMBIGUOUS — the turn may be
// running. The adapter must keep the run and return a non-nil error (uncertain),
// not a refusal code.
func TestB1PostSendFailureIsUncertainNotRefused(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111b02"}

	// Install a handler that responds to turn/start with a result that
	// fails to decode: "turn" should be an object, but we send a string.
	srv.setTurnStartResponse(`{"jsonrpc":"2.0","id":%s,"result":{"turn":"u1"}}`)

	type admResult struct {
		adm core.Admission
		err error
	}
	done := make(chan admResult, 1)
	go func() {
		adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		done <- admResult{adm, err}
	}()
	<-srv.calls // turn/start arrives; the malformed response is sent

	select {
	case r := <-done:
		// Post-send: non-nil error (uncertain), no refusal code.
		if r.err == nil {
			t.Fatal("post-send failure returned no error (must be uncertain) (B1)")
		}
		if r.adm.Code != "" {
			t.Fatalf("post-send failure returned refusal code %q (B1 — the turn may be running, must be uncertain)", r.adm.Code)
		}
		// The run must STILL be in the map (correlation preserved).
		att.mu.Lock()
		_, hasRun := att.runs[key]
		att.mu.Unlock()
		if !hasRun {
			t.Fatal("post-send failure dropped the run (B1 — correlation must survive so Lookup can resolve it)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submit did not return after post-send failure (B1)")
	}
}

// TestB3FastCompletionDoesNotWedgeAdapter reproduces B3
// (agent-message-queue-611.22.35): if the read pump processes the
// confirming notification, the completed result, and the final idle-status
// before the submitting goroutine resumes from turn/start, the adapter must
// NOT install activeTurn/status for an already-terminal run — that would
// wedge it permanently busy with no later event to clear it.
func TestB3FastCompletionDoesNotWedgeAdapter(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111b03"}

	// Install a handler that delays the turn/start RPC response until the
	// test sends the completion notifications. This simulates the race where
	// the read pump processes notifications before the Submit goroutine
	// processes the RPC response.
	srv.setTurnStartDelay(func() {
		// Send confirming notification + completion + idle before the RPC
		// response is written.
		srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
		srv.notify(t, "item/completed", `{"threadId":"t1","turnId":"u1","completedAtMs":1,"item":{"type":"agentMessage","id":"i2","text":"done"}}`)
		srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"u1","status":"completed"}}`)
		srv.notify(t, "thread/idle", `{"threadId":"t1","status":"idle"}`)
	})

	type admResult struct {
		adm core.Admission
		err error
	}
	done := make(chan admResult, 1)
	go func() {
		adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		done <- admResult{adm, err}
	}()
	<-srv.calls // turn/start arrives; the delay handler fires notifications

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("fast completion returned error: %v (B3)", r.err)
		}
		if r.adm.RunID == "" {
			t.Fatal("fast completion returned no run id (B3)")
		}
		// The adapter must NOT be left busy — activeTurn should not be set
		// for a terminal run.
		att.mu.Lock()
		activeTurn := att.activeTurn
		att.mu.Unlock()
		if activeTurn != "" {
			t.Fatalf("adapter left busy with activeTurn=%q after fast completion (B3 — would wedge forever)", activeTurn)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submit did not return after fast completion (B3 — adapter wedged)")
	}
}

// TestB6HistoryRecoveredRunHasEpochForCancel verifies Pro r2 B6 / packet 9
// (agent-message-queue-611.22.36): after a restart, a run recovered through
// lookupHistory must carry the requested epoch so CancelExact issues the
// interrupt instead of returning noop_already_terminal for a LIVE run.
//
// Scenario: the attachment has NO in-memory run (restart). thread/read returns
// a turn with our clientId in running state. Lookup installs the run. Then
// CancelExact(key, epoch) must find the run, match the epoch, and issue
// turn/interrupt — NOT return CancelNoopTerminal.
func TestB6HistoryRecoveredRunHasEpochForCancel(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111b06"}
	epoch := s.Epoch

	// Set up thread/read to return a running turn with our clientId.
	srv.setThreadReadHandler(func() string {
		return `{"thread":{"turns":[{"id":"turn-running","status":"inProgress","items":[{"type":"userMessage","id":"i1","clientId":"` + clientIDFor(key) + `","content":[]}]}]}}`
	})

	// Lookup: no in-memory run, falls through to lookupHistory, installs the
	// recovered run. The run MUST carry the requested epoch.
	ev, err := att.Lookup(key, epoch)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ev.Known {
		t.Fatal("Lookup returned unknown evidence")
	}

	// Verify the run has the epoch set.
	att.mu.Lock()
	r, ok := att.runs[key]
	att.mu.Unlock()
	if !ok {
		t.Fatal("Lookup did not install a run")
	}
	if r.epoch != epoch {
		t.Fatalf("recovered run epoch = %q, want %q (B6 — history must stamp the requested epoch)", r.epoch, epoch)
	}

	// CancelExact must issue the interrupt, not return noop_already_terminal.
	ce, err := att.CancelExact(key, epoch)
	if err != nil {
		t.Fatalf("CancelExact: %v", err)
	}
	if ce.Disposition == protocol.CancelNoopTerminal {
		t.Fatal("CancelExact returned noop_already_terminal for a LIVE run (B6 — epoch mismatch disarmed cancellation)")
	}
	if ce.Disposition != protocol.CancelRequested {
		t.Fatalf("CancelExact disposition = %v, want CancelRequested (interrupt sent)", ce.Disposition)
	}

	// Verify turn/interrupt was actually called.
	interruptSeen := false
	for {
		select {
		case c := <-srv.calls:
			if c.Method == "turn/interrupt" {
				interruptSeen = true
			}
		default:
			goto done
		}
	}
done:
	if !interruptSeen {
		t.Fatal("turn/interrupt was never sent (B6 — CancelExact must issue the interrupt for a live recovered run)")
	}
}

// TestB8aApprovalStateNotTornDownBeforeSend verifies Pro r2 #20 / packet 8a
// (agent-message-queue-611.22.36): Respond must not clear approvalReqs or
// r.interaction before client.Respond succeeds. A transport failure must
// leave the state intact so a retry can send.
func TestB8aApprovalStateNotTornDownBeforeSend(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithApprovals(true))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111b08"}
	epoch := s.Epoch

	// Submit and drive to a running turn with an interaction.
	done := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		done <- err
	}()
	<-srv.calls // turn/start arrives
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Send a server request (approval question). This is a JSON-RPC request
	// (has ID + method), not a notification.
	srv.sendServerRequest(t, "1", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"tool1","command":{"prompt":"approve?"},"availableDecisions":["accept","decline"]}`)

	// 7xl: was a 50ms sleep. Deterministic: wait until the req worker has
	// processed the approval and registered the interaction.
	waitInteraction(t, att, key)

	// Verify the interaction is set.
	att.mu.Lock()
	r, ok := att.runs[key]
	if !ok || r.interaction == nil {
		att.mu.Unlock()
		t.Fatal("interaction not set after notification")
	}
	interactionID := r.interaction.InteractionID
	att.mu.Unlock()

	// Close the connection so Respond fails (write to closed conn).
	_ = srv.ws.close()
	<-att.client.Load().Done()

	// Respond must fail (connection is closed). The specific error/code
	// depends on the transport; the key assertion is below: state must be
	// intact for retry.
	_, _ = att.Respond(key, epoch, interactionID, "accept")

	// The state must STILL be intact — retry must find the interaction.
	att.mu.Lock()
	r, ok = att.runs[key]
	if !ok {
		att.mu.Unlock()
		t.Fatal("run disappeared after failed Respond")
	}
	if r.interaction == nil {
		att.mu.Unlock()
		t.Fatal("interaction was cleared before send succeeded (B8a — state must be intact on failure so retry can send)")
	}
	if _, hasReq := r.approvalReqs[interactionID]; !hasReq {
		att.mu.Unlock()
		t.Fatal("approvalReq was deleted before send succeeded (B8a — state must be intact on failure so retry can send)")
	}
	att.mu.Unlock()
}

// TestB3TerminalMemoEvictionIsFIFO pins the eviction mechanism (packet 10
// recut 10b, agent-message-queue-611.22.36): the original eviction walked
// the map with Go's randomized iteration and deleted the FIRST key it saw
// while the comment claimed "oldest" — so it could evict the turn whose RPC
// response is still in flight, reinstating the B3 wedge intermittently.
// The memo must evict strictly in insertion order (FIFO via
// terminalTurnOrder): each new entry beyond maxLiveRuns evicts exactly the
// oldest observation and nothing else.
func TestB3TerminalMemoEvictionIsFIFO(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	memoLen := func() int {
		att.mu.Lock()
		defer att.mu.Unlock()
		return len(att.terminalTurns)
	}
	waitMemoFor := func(t *testing.T, att *Attachment, id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			att.mu.Lock()
			ok := att.terminalTurns[id]
			att.mu.Unlock()
			if ok {
				return
			}
			runtime.Gosched()
		}
		t.Fatalf("terminalTurns never recorded %s", id)
	}
	waitMemo := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if memoLen() == n {
				return
			}
			runtime.Gosched()
		}
		t.Fatalf("terminalTurns never reached %d entries (have %d)", n, memoLen())
	}
	notifyCompleted := func(id string) {
		t.Helper()
		srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"`+id+`","status":"completed"}}`)
	}

	// Prefill the memo to its bound.
	for i := 0; i < maxLiveRuns; i++ {
		notifyCompleted(fmt.Sprintf("t%02d", i))
	}
	waitMemo(maxLiveRuns)

	// Each further completion must evict exactly the OLDEST entry. With the
	// randomized-map eviction this fails with probability 16/17 on the first
	// round already.
	for n := maxLiveRuns; n < maxLiveRuns+50; n++ {
		newest := fmt.Sprintf("t%02d", n)
		oldest := fmt.Sprintf("t%02d", n-maxLiveRuns)
		notifyCompleted(newest)
		// Wait until the NEWEST entry has landed (the memo was already full,
		// so len alone cannot tell us the notification was processed), then
		// the eviction for it has happened too.
		waitMemoFor(t, att, newest)
		att.mu.Lock()
		if att.terminalTurns[oldest] {
			att.mu.Unlock()
			t.Fatalf("round %d: oldest entry %s still memoized after evicting for %s (eviction is not FIFO)", n, oldest, newest)
		}
		for i := n - maxLiveRuns + 1; i <= n; i++ {
			id := fmt.Sprintf("t%02d", i)
			if !att.terminalTurns[id] {
				att.mu.Unlock()
				t.Fatalf("round %d: entry %s was evicted but is not the oldest (random eviction dropped a possibly-live turn)", n, id)
			}
		}
		att.mu.Unlock()
	}
}

// TestB3RPCResponseGuardClosesWindowBeforeLookup pins the RPC-response arm
// of the B3 fix (packet 10 recut 10c, agent-message-queue-611.22.36): the
// memo guard exists to close the race window BEFORE any Lookup is made, but
// the aggregate end-to-end test only checked Inspect AFTER Lookup, so
// reverting the memo guard alone (or the lookupHistory activeTurn clear
// alone) still passed. This test drives the missed-confirmation race and
// asserts Inspect is not busy and activeTurn is empty with NO Lookup call
// at all.
//
// The "error" row is 10a: the memo's status whitelist was narrower than the
// terminality the status switch implements (completed/interrupted/
// default->StateFailed), so a turn ending with an unrecognized status was
// not memoized and the wedge returned. turn/completed is recorded
// unconditionally — the notification itself means the turn is over.
func TestB3RPCResponseGuardClosesWindowBeforeLookup(t *testing.T) {
	for _, tc := range []struct {
		status, requestID string
	}{
		{"completed", "11111111-1111-4111-8111-111111111b3c"},
		{"error", "11111111-1111-4111-8111-111111111b3a"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			sock, srv := startFakeAppServer(t)
			att, err := Attach(sock, "t1", WithConfirmTimeout(200*time.Millisecond))
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			t.Cleanup(func() { _ = att.Close() })
			<-srv.calls // initialize
			<-srv.calls // thread/resume

			s := att.Inspect()
			key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: tc.requestID}
			// The confirming userMessage item is MISSED: the turn completes
			// before the turn/start response reaches Submit.
			srv.setTurnStartResponse(`{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn1","status":"inProgress"}}}`)
			srv.setTurnStartDelay(func() {
				srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"turn1"}}`)
				srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"turn1","status":"`+tc.status+`"}}`)
				srv.notify(t, "thread/status/changed", `{"threadId":"t1","status":{"type":"idle"}}`)
			})
			done := make(chan struct{})
			go func() {
				_, _ = att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("submit did not return after missed confirmation (B3)")
			}

			// NO Lookup, NO thread/read handler installed: if this passes, the
			// memo guard (not history reconciliation) closed the window.
			if insp := att.Inspect(); insp.Status == "busy" {
				t.Fatal("Inspect reports busy with NO Lookup called (the RPC-response memo guard did not close the window)")
			}
			att.mu.Lock()
			active := att.activeTurn
			att.mu.Unlock()
			if active != "" {
				t.Fatalf("activeTurn = %q, want empty with NO Lookup called (RPC response reinstated a finished turn as active)", active)
			}
		})
	}
}

// TestB3LookupHistoryMemoStaysBounded pins the FIFO bound THROUGH the
// lookupHistory write path (packet 10d, agent-message-queue-611.22.36):
// lookupHistory recorded turns into the terminalTurns map directly,
// bypassing the FIFO. Those entries never entered terminalTurnOrder, so the
// eviction — which tests len(terminalTurnOrder), the SLICE — never noticed
// the MAP exceeding the bound: every history-recovered terminal turn leaked
// one memo entry for the life of the attachment. Both memo writers must go
// through the one owner.
func TestB3LookupHistoryMemoStaysBounded(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	// Drain srv.calls: with more lookups than the channel buffer the fake
	// server goroutine would block on send and stop answering.
	go func() {
		for range srv.calls {
		}
	}()

	s := att.Inspect()
	targetID := s.TargetID

	// thread/read resolves maxLiveRuns DISTINCT completed turns, each with
	// its own clientId, so each Lookup memoizes a different turn id through
	// the lookupHistory write path (no in-memory run exists for any key).
	srv.setThreadReadHandler(func() string {
		var b strings.Builder
		b.WriteString(`{"thread":{"turns":[`)
		for i := 0; i < maxLiveRuns; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			cid := clientIDFor(requests.Key{CreatorHost: "local", TargetID: targetID, RequestID: fmt.Sprintf("%08x-1111-4111-8111-1111111110b2", i)})
			b.WriteString(`{"id":"h` + fmt.Sprintf("%03d", i) + `","status":"completed","items":[{"type":"userMessage","id":"i` + fmt.Sprintf("%d", i) + `","clientId":"` + cid + `","content":[]},{"type":"agentMessage","id":"a` + fmt.Sprintf("%d", i) + `","text":"done"}]}`)
		}
		b.WriteString(`]}}`)
		return b.String()
	})

	for i := 0; i < maxLiveRuns; i++ {
		key := requests.Key{CreatorHost: "local", TargetID: targetID, RequestID: fmt.Sprintf("%08x-1111-4111-8111-1111111110b2", i)}
		if _, err := att.Lookup(key, "epoch-1"); err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
	}

	// One more terminal observation through the pump path: this must evict
	// the OLDEST entry overall — h000, memoized first via lookupHistory.
	oldest := "h000"
	att.mu.Lock()
	wasPresent := att.terminalTurns[oldest]
	att.mu.Unlock()
	if !wasPresent {
		t.Fatalf("oldest history-recovered turn %s was never memoized (lookupHistory path not exercising the memo)", oldest)
	}
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"turn-new","status":"completed"}}`)
	// 7xl: was an inline 2ms-sleep poll; waitMemoForID is the same wait,
	// shared and deadline-bounded.
	waitMemoForID(t, att, "turn-new")

	att.mu.Lock()
	n := len(att.terminalTurns)
	order := len(att.terminalTurnOrder)
	gone := !att.terminalTurns[oldest]
	att.mu.Unlock()
	if n != maxLiveRuns {
		t.Fatalf("terminalTurns has %d entries after maxLiveRuns history recoveries plus one completed, want exactly %d (the lookupHistory write path bypasses the FIFO bound)", n, maxLiveRuns)
	}
	if order != n {
		t.Fatalf("terminalTurnOrder (%d) and terminalTurns (%d) disagree — the memo and its FIFO are out of sync", order, n)
	}
	if !gone {
		t.Fatalf("oldest entry %s (memoized via lookupHistory) survived the eviction for turn-new — the FIFO does not own lookupHistory's entries", oldest)
	}
}

// TestBK4AckedRunsAreBounded is the 611.22.19 BK4 happy path for the codex
// attachment's in-memory maps: terminal+acknowledged runs are pruned from
// runs/byClientID/byTurn once the acked FIFO exceeds maxLiveRuns, so the
// correlation maps cannot grow without bound. The durable store remains the
// source of truth; a dropped acked run Lookup returns EvidenceNone exactly as
// a compacted tombstone would.
func TestBK4AckedRunsAreBounded(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()

	// Drive maxLiveRuns+1 distinct runs to terminal+acked.
	for i := 0; i < maxLiveRuns+1; i++ {
		key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: fmt.Sprintf("11111111-1111-4111-8111-11111111%04d", i)}
		turnID := fmt.Sprintf("bk%04d", i)
		srv.setTurnStartResponse(`{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"` + turnID + `","status":"inProgress"}}}`)
		done := make(chan struct{})
		go func() {
			_, _ = att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "work"}})
			close(done)
		}()
		<-srv.calls // turn/start
		srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"`+turnID+`"}}`)
		srv.notify(t, "item/started", `{"threadId":"t1","turnId":"`+turnID+`","item":{"type":"userMessage","id":"i`+turnID+`","clientId":"`+clientIDFor(key)+`","content":[]}}`)
		<-done
		srv.notify(t, "item/completed", `{"threadId":"t1","turnId":"`+turnID+`","completedAtMs":1,"item":{"type":"agentMessage","id":"o`+turnID+`","text":"OUT"}}`)
		srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"`+turnID+`","status":"completed"}}`)
		waitMemoForID(t, att, turnID)

		ev, _ := att.Lookup(key, s.Epoch)
		if ev.State != protocol.StateCompleted || ev.Result == nil {
			t.Fatalf("run %d not terminal before ack: %+v", i, ev)
		}
		att.AcknowledgeResult(key, s.Epoch, protocol.EvidenceDigest(ev.Result))
	}

	att.mu.Lock()
	got := len(att.runs)
	att.mu.Unlock()

	// The FIFO cap drops the oldest acked run; the maps never exceed
	// maxLiveRuns. Without the BK4 prune, all maxLiveRuns+1 entries would
	// persist forever.
	if got != maxLiveRuns {
		t.Fatalf("runs map unbounded after ack: want %d, got %d", maxLiveRuns, got)
	}

	// The oldest acked run is gone from the maps; its Lookup still returns
	// EvidenceNone (acked semantics), not an error.
	oldest := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111110000"}
	att.mu.Lock()
	_, oldestPresent := att.runs[oldest]
	att.mu.Unlock()
	if oldestPresent {
		t.Fatal("oldest acked run was not pruned from runs map")
	}
	lk, err := att.Lookup(oldest, s.Epoch)
	if err != nil {
		t.Fatalf("lookup of pruned acked run: %v", err)
	}
	if lk.Class != core.EvidenceNone {
		t.Fatalf("pruned acked run Lookup: want EvidenceNone, got class=%s", lk.Class)
	}
}

// 611.56 (live 2026-10-06, codex-cli 0.160): a thread under the owner's
// approvals_reviewer = guardian_subagent had its approvals answered by the
// reviewer, and none reached the Buzz DM. Inspect names the reviewer from
// thread/resume and follows thread/settings/updated.
func TestInspectNamesTheAutomaticApprovalsReviewer(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	srv.setResumeReviewer("guardian_subagent")
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	if got := att.Inspect().ApprovalReviewer; got != "guardian_subagent" {
		t.Fatalf("after resume: approval reviewer %q, want guardian_subagent", got)
	}
	srv.notify(t, "thread/settings/updated", `{"threadId":"t1","threadSettings":{"approvalsReviewer":"user"}}`)
	deadline := time.Now().Add(5 * time.Second)
	for att.Inspect().ApprovalReviewer != "" {
		if time.Now().After(deadline) {
			t.Fatalf("after settings update to user: approval reviewer %q, want empty", att.Inspect().ApprovalReviewer)
		}
		runtime.Gosched()
	}
}
