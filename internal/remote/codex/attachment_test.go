package codex

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	ws    *wsConn
	calls chan rpcMessage
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
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{"turn":{"id":"u1","status":"inProgress"}}}`))
			case "turn/interrupt":
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{}}`))
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
	if call.Method != "turn/start" || params["clientUserMessageId"] != key.RequestID {
		t.Fatalf("unexpected native call: %s %v", call.Method, params)
	}
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+key.RequestID+`","content":[]}}`)
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
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+key.RequestID+`","content":[]}}`)
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
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+key.RequestID+`","content":[]}}`)
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
	att.Lookup(key, s.Epoch)
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
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+key.RequestID+`","content":[]}}`)
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
