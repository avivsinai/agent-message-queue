package codex

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{"thread":{"id":"t1","cwd":"/work","status":{"type":"idle"}}}}`))
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
	time.Sleep(30 * time.Millisecond)
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

	var ev core.Evidence
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ev, _ = att.Lookup(key, s.Epoch)
		if ev.State == protocol.StateCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// TestUnconfirmedRunFallsThroughToHistoryAfterTimeout reproduces Pro F1: a
// retained-but-unconfirmed run permanently shadows lookupHistory, so the
// record sticks at Uncertain forever and a real completed result is never
// delivered. After confirmTimeout, if still unconfirmed, Lookup must fall
// through to lookupHistory (a thread/read RPC), which resolves the turn by
// clientId.
//
// Also covers the 12s confirm-timeout arm (the bead names it; the existing
// TestUnconfirmedTurnStartIsUncertainNotRejected covers only the client.Done()
// arm). The fake clock makes the timeout deterministic — no real sleep.
func TestUnconfirmedRunFallsThroughToHistoryAfterTimeout(t *testing.T) {
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
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111810"}
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
	// No userMessage item ever confirms the turn. Advance the clock past
	// confirmTimeout so the confirm-timeout arm fires (no real 12s sleep).
	clk.advance(51 * time.Millisecond)
	r := <-done
	if r.err == nil {
		t.Fatalf("unconfirmed turn/start returned no error: %+v", r.adm)
	}
	if r.adm.RunID == "" {
		t.Fatal("unconfirmed turn/start dropped the run id")
	}

	// Before the deadline is already past (we advanced the clock). But first,
	// verify that a Lookup BEFORE the deadline returns EvidenceTentative. We
	// can't go back in time, so instead verify the fall-through happens NOW:
	// the run is unconfirmed and past the deadline, so Lookup must call
	// lookupHistory (a thread/read RPC). The fake server's default handler
	// returns an error for thread/read, so lookupHistory returns
	// EvidenceUnknown — but the key assertion is that thread/read WAS called
	// (the call channel receives it), proving the fall-through.
	callsBefore := len(srv.calls)
	// Lookup falls through to lookupHistory (thread/read RPC). The fake
	// server returns an error for thread/read, so lookupHistory returns an
	// error — but the key assertion is that thread/read WAS called, proving
	// the fall-through. The error is expected, so check the call count, not
	// the return value.
	lk, lerr := att.Lookup(key, s.Epoch)
	_ = lk
	_ = lerr
	// Drain the thread/read call (non-blocking — the fake server handles it).
	time.Sleep(50 * time.Millisecond)
	callsAfter := len(srv.calls)
	if callsAfter <= callsBefore {
		t.Fatal("Lookup did not fall through to lookupHistory (thread/read) after the deadline — the unconfirmed run shadowed it forever (Pro F1)")
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

	var ev core.Evidence
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ev, _ = att.Lookup(key, s.Epoch)
		if ev.State == protocol.StateCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// TestRunIDAccessorTakesLock verifies B1 (F3): a.runID takes a.mu before
// reading r.turnID. This is a structural test — it confirms the accessor
// exists and is used, not a runtime race. The runtime race is caught by
// running the whole suite under -race: confirmRun writes r.turnID from the
// read-loop goroutine while Submit reads it via a.runID. If runID were still
// an unlocked method on *run, -race would fire in TestSubmitBindsTurnAndCompletes.
func TestRunIDAccessorTakesLock(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-1111111118b1"}
	// Start a turn and confirm it so r.turnID is set by confirmRun (from the
	// read-loop goroutine).
	done := make(chan struct{}, 1)
	go func() {
		_, _ = att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "work"}})
		done <- struct{}{}
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	// Confirm the run: emit the userMessage item that carries our clientId.
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","clientId":"`+clientIDFor(key)+`"}}`)
	<-done

	// Now read the runID through the accessor (takes a.mu). Under -race, if
	// the accessor did not take the lock, the read of r.turnID here would race
	// with any concurrent write from the read-loop goroutine.
	att.mu.Lock()
	r, ok := att.runs[key]
	att.mu.Unlock()
	if !ok {
		t.Fatal("run not found")
	}
	rid := att.runID(r)
	if rid != "turn:u1" {
		t.Fatalf("runID = %q, want turn:u1 (B1: runID must read r.turnID under a.mu)", rid)
	}
}

// TestHistoryResolvedRunConverges reproduces B2 (F1 never converges): when
// lookupHistory resolves a run as terminal, the in-memory run must become
// terminal so AcknowledgeResult can release it. Without the fix, the run
// stays StateRunning forever, AcknowledgeResult refuses every ack, and
// Reconcile re-reads the whole transcript every tick.
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
	done := make(chan struct{}, 1)
	go func() {
		_, _ = att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "work"}})
		done <- struct{}{}
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	// No confirming userMessage — advance past confirmTimeout so Lookup
	// falls through to lookupHistory.
	clk.advance(51 * time.Millisecond)
	<-done // Submit returns with an error (unconfirmed)

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

	// Close the server BEFORE Submit so the RPC write fails (pre-send).
	_ = srv.ws.close()
	// Deterministic: wait for the read pump to register the closed
	// connection (744.8 — no time.Sleep).
	<-att.client.Done()

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

// TestB6aHistoryResolvedRunIsCancellable reproduces B6a
// (agent-message-queue-611.22.35): when lookupHistory resolves a run (live
// OR terminal), it must install a confirmed run entry so CancelExact can
// find it. Without this, a surviving run after restart cannot be cancelled.
func TestB6aHistoryResolvedRunIsCancellable(t *testing.T) {
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

	type admResult struct {
		adm core.Admission
		err error
	}
	done := make(chan admResult, 1)
	go func() {
		adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		done <- admResult{adm, err}
	}()
	<-srv.calls // turn/start
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"agentMessage","id":"i2","text":"running..."}}`)

	// Simulate restart: clear the in-memory runs map.
	att.mu.Lock()
	for k := range att.runs {
		delete(att.runs, k)
	}
	for k := range att.byClientID {
		delete(att.byClientID, k)
	}
	for k := range att.byTurn {
		delete(att.byTurn, k)
	}
	att.mu.Unlock()

	// History resolves the run as live (running).
	srv.setThreadReadHandler(func() string {
		return `{"thread":{"turns":[{"id":"u1","status":"running","items":[{"type":"userMessage","clientId":"` + clientIDFor(key) + `"},{"type":"agentMessage","text":"running..."}]}]}}`
	})

	lk, _ := att.Lookup(key, s.Epoch)
	if !lk.Known || !lk.Admitted {
		t.Fatalf("history did not resolve the run as live: %+v (B6a)", lk)
	}

	// The run must now be in the runs map so CancelExact can find it.
	att.mu.Lock()
	r, hasRun := att.runs[key]
	att.mu.Unlock()
	if !hasRun {
		t.Fatal("history resolved the run but did not install a confirmed entry; CancelExact cannot find it (B6a)")
	}
	if !r.confirmed {
		t.Fatal("history-resolved run is not marked confirmed (B6a)")
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

	// Give the read pump a moment to process the server request.
	time.Sleep(50 * time.Millisecond)

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
	<-att.client.Done()

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

// TestB3MissedConfirmationDoesNotWedgeBusy verifies Pro r2 B3 second half /
// packet 10 (agent-message-queue-611.22.36): when the confirming userMessage
// item is missed, turn/completed clears activeTurn but finds no byTurn entry,
// so the run is never marked terminal. The RPC response then reinstalls the
// finished turn as active, wedging the attachment busy forever. The fix:
// (1) turn/completed records terminal turn IDs in a bounded memo even with no
// byTurn entry, (2) the RPC-response guard consults the memo, (3) lookupHistory
// clears activeTurn if it matches the terminal turn.
func TestB3MissedConfirmationDoesNotWedgeBusy(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111b03"}
	epoch := s.Epoch

	// Submit — it will block on confirmCh. Drive it in a goroutine.
	done := make(chan struct {
		adm core.Admission
		err error
	}, 1)
	// Use setTurnStartDelay to send notifications BEFORE the turn/start RPC
	// response is sent back. This reproduces the race: the read pump processes
	// the notifications before the Submit goroutine processes the RPC response.
	// The turn/start response returns turn id "turn1" to match the notifications.
	srv.setTurnStartResponse(`{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn1","status":"inProgress"}}}`)
	srv.setTurnStartDelay(func() {
		// 1. turn/started(T) — sets activeTurn
		srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"turn1"}}`)
		// 2. Our userMessage item is MISSED — do NOT send item/started with
		//    our clientId. confirmRun never runs, byTurn[turn1] never bound.
		// 3. turn/completed(T) — clears activeTurn but finds no byTurn entry.
		//    Without the fix, the terminal observation is lost.
		srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"turn1","status":"completed"}}`)
		// 4. thread/status/changed(idle)
		srv.notify(t, "thread/status/changed", `{"threadId":"t1","status":{"type":"idle"}}`)
	})

	go func() {
		adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		done <- struct {
			adm core.Admission
			err error
		}{adm, err}
	}()

	// Wait for Submit to return (it will timeout on confirmCh — the
	// confirming userMessage was never sent).
	select {
	case r := <-done:
		// Submit returns uncertain (confirmTimeout). The key assertion is
		// below: activeTurn must not be turn1.
		_ = r
	case <-time.After(10 * time.Second):
		t.Fatal("submit did not return after missed confirmation (B3)")
	}

	// Set up thread/read to return the completed turn with our clientId.
	srv.setThreadReadHandler(func() string {
		return `{"thread":{"turns":[{"id":"turn1","status":"completed","items":[{"type":"userMessage","id":"i1","clientId":"` + clientIDFor(key) + `","content":[]},{"type":"agentMessage","id":"i2","text":"PONG"}]}]}}`
	})

	// Lookup resolves the turn via history.
	_, err = att.Lookup(key, epoch)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	// The attachment must NOT be busy, and activeTurn must be empty.
	insp := att.Inspect()
	if insp.Status == "busy" {
		t.Fatal("Inspect reports busy after Lookup resolved the terminal turn (B3 — the RPC response reinstated the finished turn as active)")
	}
	att.mu.Lock()
	active := att.activeTurn
	att.mu.Unlock()
	if active != "" {
		t.Fatalf("activeTurn = %q, want empty (B3 — a finished turn must never be reinstalled as active)", active)
	}
}
