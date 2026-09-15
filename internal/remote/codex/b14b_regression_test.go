package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// b14bDeadline bounds every blocking wait: a regression FAILS, never hangs CI.
const b14bDeadline = 5 * time.Second

// newB14bClient dials a stand-in app-server over a unix socket and wires a
// handler-invocation counter into both callback paths.
func newB14bClient(t *testing.T, onNotif func(), onReq func(ServerRequest)) (*Client, net.Listener) {
	t.Helper()
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in server accepts one connection and then just reads
	// (discarding) — it never sends anything unless a test writes itself.
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		ws, err := acceptServerWS(conn)
		if err != nil {
			return
		}
		for {
			if _, err := ws.readText(); err != nil {
				return
			}
		}
	}()
	ws, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	h := &swappableHandlers{}
	if onNotif != nil {
		h.setNote(func(n Notification) { onNotif() })
	}
	if onReq != nil {
		h.setReq(onReq)
	}
	c := newClient(ws, h.handlers())
	attachTestHandlers(c, h)
	t.Cleanup(func() { _ = c.Close(); _ = l.Close() })
	return c, l
}

// newB14bClientWithServer builds a client whose stand-in server forwards the
// FIRST frame the client sends back to the test via onFrame.
func newB14bClientWithServer(t *testing.T, onFrame func([]byte)) (*Client, net.Listener) {
	t.Helper()
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		ws, err := acceptServerWS(conn)
		if err != nil {
			return
		}
		first := true
		for {
			payload, err := ws.readText()
			if err != nil {
				return
			}
			var msg rpcMessage
			if json.Unmarshal(payload, &msg) == nil && msg.ID != nil && msg.Method != "" {
				// A Call ("ping"): answer it so pump-liveness assertions can
				// prove the read pump still moves.
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":{}}`))
				continue
			}
			if first {
				first = false
				onFrame(payload)
			}
		}
	}()
	ws, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	h := &swappableHandlers{}
	c := newClient(ws, h.handlers())
	attachTestHandlers(c, h)
	t.Cleanup(func() { _ = c.Close() })
	return c, l
}

// sendNotif / sendReq let tests inject reader-side frames as if the server
// sent them — but the real readLoop is the only writer-safe reader of the
// socket, so tests instead deliver via a direct server frame over the conn.
// Simplest deterministic injection: call the dispatch paths the readLoop
// would call, since B9/B6/B8 target those paths' lifecycle, not the parse.

// TestB14bCloseStopsReqWorker pins B9 (recut: reqWorker is the only worker):
// Close runs the full teardown — the req worker exits (reqWorkerDone) after
// the read pump is drained, and the queues close safely.

func TestB14bCloseStopsReqWorker(t *testing.T) {
	c, _ := newB14bClient(t, nil, nil)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-c.reqWorkerDone:
	case <-time.After(b14bDeadline):
		t.Fatal("reqWorker did not exit after Close (B9 regression)")
	}
}

// TestB14bWaitCallbacksCountsInFlight pins the drain: waitCallbacks must
// await the EXECUTING server-request handler (reqInFlight), not just queue
// depth. Close stays bounded even against a wedged handler.
func TestB14bWaitCallbacksCountsInFlight(t *testing.T) {
	release := make(chan struct{})
	handlerRunning := make(chan struct{})
	c, _ := newB14bClient(t, nil, nil)
	handlersOf(c).setReq(func(r ServerRequest) {
		close(handlerRunning)
		<-release
	})
	c.dispatchServerRequest(ServerRequest{ID: json.RawMessage(`"srv-1"`), Method: "m"})
	<-handlerRunning
	done := make(chan struct{})
	go func() { c.waitCallbacks(); close(done) }()
	// waitCallbacks must NOT return while the handler is executing.
	select {
	case <-done:
		t.Fatal("waitCallbacks returned while handler still executing (in-flight not tracked)")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(b14bDeadline):
		t.Fatal("waitCallbacks never returned after in-flight handler finished")
	}
}

// TestB14bCloseTwiceSafe pins B9 idempotency: Close twice must not panic
// (separate stop-Once, idempotent ws close).
func TestB14bCloseTwiceSafe(t *testing.T) {
	c, _ := newB14bClient(t, nil, nil)
	_ = c.Close()
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(b14bDeadline):
		t.Fatal("second Close deadlocked")
	}
}

// TestB14bCallWaitsBoundedBehindWedgedWriter pins B6: ctx must bound WAITING
// for the writer slot, not just the write. A wedged writer (server never
// reads) holds the slot; a second Call with a short deadline must return
// ctx.Err within its deadline instead of blocking behind the wedged writer.
func TestB14bCallWaitsBoundedBehindWedgedWriter(t *testing.T) {
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	// The server accepts but NEVER reads: the client's first big write wedges.
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = acceptServerWS(conn)
		// deliberately never read or write
	}()
	ws, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := newClient(ws, Handlers{})
	t.Cleanup(func() { _ = c.Close() })

	// 7xl-r2 (F761-1, yoetz round): NO second writer. An earlier draft kept
	// a "Writer A" goroutine alongside the test-held slot; it blocked in
	// lockWrite's unconditional channel send forever (socket close does not
	// unblock a channel send) — a permanently leaked goroutine, confirmed
	// empirically by the lead's goroutine-leak probe (2 before, 3 after).
	// The test-owned slot already establishes the exact contention Call B
	// contends on; a second writer adds nothing and leaks.
	//
	// 7xl (agent-message-queue-7xl): was a 150ms sleep to "give the writer
	// time to acquire the slot". Deterministic: we hold the writer slot
	// OURSELVES before any other goroutine can take it — no timing
	// assumption at all. Call B's ctx proving the bounded wait is what this
	// test pins.
	ws.wmu <- struct{}{} // hold the slot for the whole test

	// Call B: small payload, 250ms deadline. The slot is held by the test
	// (uncontested under the old mutex: B waited unboundedly).
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err = c.Call(ctx, "ping", map[string]any{}, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("call B unexpectedly succeeded while writer wedged")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("call B blocked %v — ctx did not bound WAITING for the writer (B6 regression)", elapsed)
	}
	// Under -race scheduling, B's ctx race may lose to the read pump's EOF
	// (Close tears the conn down when the test ends); the INVARIANT is that
	// B returned within its deadline with a bounded error, never blocked
	// behind A. DeadlineExceeded is the expected case on a live connection.
	c.mu.Lock()
	readErr := c.readErr
	c.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) && (readErr == nil || !errors.Is(err, readErr)) {
		t.Fatalf("call B err = %v, want context.DeadlineExceeded (or the connection-closed error under teardown)", err)
	}
}

// TestB14bReplyRequiredNotDroppedAndPumpStaysLive pins B8: notifications may
// drop on a full backlog, but reply-required server requests must never be
// silently dropped — delivered via their own queue, or explicitly failed on
// the should-be-impossible overflow — and the read pump never blocks on
// handler latency.
func TestB14bReplyRequiredNotDroppedAndPumpStaysLive(t *testing.T) {
	var mu sync.Mutex
	approved := make(chan string, 8)
	blockApproval := make(chan struct{}) // wedges the first approval handler
	c, _ := newB14bClient(t, nil, nil)
	first := true
	handlersOf(c).setReq(func(r ServerRequest) {
		mu.Lock()
		approved <- r.Method
		mu.Unlock()
		if first {
			first = false
			<-blockApproval // wedge AFTER recording: the pump must survive it
		}
	})

	// A reply-required request with a wedged handler: it is still DELIVERED
	// (queued) and the read pump must stay live — an RPC Call completes even
	// though the approval handler is stuck (the pump never blocks on handler
	// latency; notifications run synchronously but are bounded state-applies).
	c.dispatchServerRequest(ServerRequest{ID: json.RawMessage(`"srv-1"`), Method: "item/commandExecution/requestApproval"})
	select {
	case m := <-approved:
		if m != "item/commandExecution/requestApproval" {
			t.Fatalf("delivered method = %s", m)
		}
	case <-time.After(b14bDeadline):
		t.Fatal("reply-required server request dropped (B8 regression)")
	}
	close(blockApproval)

	// Overflow (should-be-impossible): fill reqQ past capacity with a wedged
	// handler, the overflowed request must be EXPLICITLY FAILED, not dropped.
	// c2's stand-in server echoes the first frame it reads, so respondError's
	// explicit error is observable without touching the live client's socket
	// (swapping it would race the readLoop).
	release2 := make(chan struct{})
	respCh := make(chan []byte, 1)
	c2, _ := newB14bClientWithServer(t, func(payload []byte) { respCh <- payload })
	headPicked := make(chan struct{})
	handlersOf(c2).setReq(func(r ServerRequest) {
		// Signal that the req worker has consumed the head from reqQ, then
		// wedge: the queue below can now fill deterministically.
		close(headPicked)
		<-release2
	})
	c2.dispatchServerRequest(ServerRequest{ID: json.RawMessage(`"srv-head"`), Method: "m"})
	<-headPicked // worker consumed the head; it is wedged in this handler
	for i := 0; i < cap(c2.reqQ)+1; i++ {
		c2.dispatchServerRequest(ServerRequest{ID: json.RawMessage(`"srv-x"`), Method: "m"})
	}
	select {
	case payload := <-respCh:
		var msg rpcMessage
		if err := json.Unmarshal(payload, &msg); err != nil || msg.Error == nil {
			t.Fatalf("overflow response not an explicit error: %s (%v)", payload, err)
		}
		if msg.ID == nil || string(*msg.ID) != `"srv-x"` {
			t.Fatalf("error response id = %s, want srv-x", string(*msg.ID))
		}
	case <-time.After(b14bDeadline):
		t.Fatal("overflowed reply-required request neither delivered nor explicitly failed (B8 regression)")
	}
	// Pump liveness with c2's handler STILL wedged (release2 open): a real
	// RPC must complete — the read pump never blocks on handler latency. c2's
	// stand-in server replies to the Call, so success proves the pump moved.
	ctx, cancel := context.WithTimeout(context.Background(), b14bDeadline)
	defer cancel()
	var pong struct{}
	if err := c2.Call(ctx, "ping", map[string]any{}, &pong); err != nil {
		t.Fatalf("RPC blocked while server-request handler wedged (pump starved): %v", err)
	}
	close(release2)
}

// TestB14bCloseConcurrentWithInboundServerRequest pins the BLOCKER: Close
// must tear the socket down and DRAIN the read pump BEFORE closing reqQ.
// The old order (close reqQ -> drain -> ws.close) left the pump live during
// the drain window: an inbound approval frame then sent on a CLOSED reqQ —
// send on closed channel, no recover in readLoop, process crash.
//
// 7xl-r3 (F761-2/F761-3, yoetz recut): the r2 "barrier" was an observation,
// not a barrier — receiving the overflow frame proves an overflow happened
// EARLIER, not that the pump is still inside dispatchServerRequest when
// Close fires; and the direct c.dispatchServerRequest(wedge) could itself
// take the overflow path if the pump filled reqQ first (false barrier),
// while the N unconditional fill sends could block forever on a stolen
// slot (F761-3 hang). The r3 recut is a TWO-WAY barrier on the pump path:
//
//  1. Setup determinism: no hammer server exists during setup; the wedge
//     handler signals "parked" through a channel the test waits on; every
//     queue send is select+timeout bounded, so a competing producer can
//     never hang the test.
//  2. The pump is PARKED inside dispatchServerRequest AFTER decode and
//     BEFORE its reqQ send (testDispatchGate), with reqQ holding one
//     reserved free slot for the parked send.
//  3. Close() starts; the PEER observes socket closure (read EOF) —
//     proof ws.close() ran.
//  4. The pump is released. Correct ordering: reqQ is still open when the
//     parked pump completes its send → no panic. Reverted ordering
//     (queue closed before the pump is drained): the released pump sends
//     on the closed queue → DETERMINISTIC panic. The ordering is tested
//     directly, not by scheduling luck.
func TestB14bCloseConcurrentWithInboundServerRequest(t *testing.T) {
	dir, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	// No hammer DURING setup (r3 spec 1): the connection is accepted and
	// the websocket handshake completes immediately (dial needs it), but
	// the frame writer only starts on hammerGo — during setup the server
	// sends nothing, so no frame can compete with initialization.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	peerSawClose := make(chan struct{})
	var peerSawCloseOnce sync.Once
	hammerGo := make(chan struct{})
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		ws, err := acceptServerWS(conn)
		if err != nil {
			return
		}
		go func() {
			<-hammerGo // frames only after the barrier is armed
			frame := []byte(`{"jsonrpc":"2.0","id":"srv-1","method":"item/commandExecution/requestApproval","params":{}}`)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := ws.writeText(frame); err != nil {
					return
				}
			}
		}()
		for {
			if _, err := ws.readText(); err != nil {
				peerSawCloseOnce.Do(func() { close(peerSawClose) })
				return // client closed — correct ordering tears the socket down
			}
		}
	}()

	// Wedge-parked handshake (r3 spec 1): the worker parks inside the wedge
	// handler and signals parked; the test waits for the handshake before
	// filling, so the fill loop can never race the worker. The worker
	// stays parked until releaseWorker, so reqQ retains whatever the fill
	// puts in.
	parked := make(chan struct{})
	var parkedOnce sync.Once
	workerReleased := make(chan struct{})
	var workerReleaseOnce sync.Once
	releaseWorker := func() { workerReleaseOnce.Do(func() { close(workerReleased) }) }
	t.Cleanup(releaseWorker)
	w, err := dialUnixWS(sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &Client{
		ws:            w,
		pending:       map[int64]chan rpcMessage{},
		closed:        make(chan struct{}),
		reqQ:          make(chan ServerRequest, maxLiveRuns+reqQSlack),
		reqWorkerDone: make(chan struct{}),
		onServerRequest: func(r ServerRequest) {
			if r.Method == "wedge" {
				parkedOnce.Do(func() { close(parked) })
				<-workerReleased // park: no consumer drains reqQ past here
			}
		},
	}
	go c.readLoop()
	go c.reqWorker()

	// Park the worker: one bounded send, then the handshake.
	select {
	case c.reqQ <- ServerRequest{ID: json.RawMessage(`"wedge"`), Method: "wedge"}:
	case <-time.After(b14bDeadline):
		t.Fatal("wedge send blocked (F761-3 guard)")
	}
	select {
	case <-parked:
	case <-time.After(b14bDeadline):
		t.Fatal("worker never parked (handshake broken)")
	}

	// Fill reqQ to ONE free slot (bounded sends, F761-3): that slot is
	// reserved for the parked pump's dispatch, which the gate holds.
	for i := 0; i < maxLiveRuns+reqQSlack-1; i++ {
		select {
		case c.reqQ <- ServerRequest{ID: json.RawMessage(`"fill"`), Method: "fill"}:
		case <-time.After(b14bDeadline):
			t.Fatalf("fill send %d blocked (F761-3 guard)", i)
		}
	}

	// Arm the two-way barrier: the NEXT server request decoded by the pump
	// parks inside testDispatchGate, before its reqQ send.
	gateEntered := make(chan struct{})
	var gateEnteredOnce sync.Once
	gateReleased := make(chan struct{})
	var gateReleaseOnce sync.Once
	prevGate := setTestDispatchGate(func(sr ServerRequest) {
		gateEnteredOnce.Do(func() { close(gateEntered) })
		<-gateReleased // parked: post-decode, pre-send
	})
	t.Cleanup(func() { setTestDispatchGate(prevGate) })

	// Start the hammer: the pump decodes a frame and parks in the gate.
	close(hammerGo)
	select {
	case <-gateEntered:
	case <-time.After(b14bDeadline):
		t.Fatal("pump never reached the dispatch gate (barrier broken)")
	}

	// Un-wedge the worker BEFORE Close: the worker park existed only to
	// keep reqQ deterministic during the fill; Close's drain must be able
	// to complete. The pump stays parked at the gate regardless — that is
	// the barrier Close must respect.
	releaseWorker()

	// Start Close while the pump is parked inside dispatchServerRequest.
	closeDone := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closeDone)
	}()

	// Peer-side proof (r3 spec): ws.close() ran — the server's read gets
	// EOF while the pump is still parked at the gate and reqQ is OPEN.
	select {
	case <-peerSawClose:
	case <-time.After(b14bDeadline):
		t.Fatal("peer never observed socket closure (ws.close did not run first)")
	}

	// Release the pump. Correct ordering: reqQ is still open (Close closes
	// it only after <-c.closed, i.e. after the pump exits), so the parked
	// send completes into the reserved slot and the pump exits — Close then
	// finishes. Reverted ordering: reqQ was closed BEFORE the pump drained;
	// the released pump's send panics deterministically.
	gateReleaseOnce.Do(func() { close(gateReleased) })

	// Close must now complete: pump exited, drain bounded, reqQ closed last.
	select {
	case <-closeDone:
	case <-time.After(b14bDeadline):
		t.Fatal("Close deadlocked after gate release")
	}

	// No panic = pass. (A send-on-closed reqQ panics the whole test process.)
}

// TestB14bCloseBoundedUnderWedgedHandler pins recut L1: Close must NEVER
// hang, even against a reqWorker handler that wedges forever. The whole
// post-stopOnce drain shares one bounded budget — on timeout Close proceeds
// (the conn is already closed; the leaked handler is harmless).
func TestB14bCloseBoundedUnderWedgedHandler(t *testing.T) {
	c, _ := newB14bClient(t, nil, nil)
	// 611.22.33 test-bar: shrink the drain budget for this test. The old
	// shape burned Close's FULL 2s production budget — a real 4s of wall
	// clock per run — to prove a bound whose VALUE is irrelevant: the
	// invariant is that Close returns instead of hanging on a wedged
	// handler. With a 50ms budget the same wedged-handler scenario is
	// exercised deterministically in milliseconds, and the asserts below
	// scale with it.
	const budget = 50 * time.Millisecond
	c.closeDrainBudget.Store(budget)
	handlerStarted := make(chan struct{})
	neverRelease := make(chan struct{}) // wedged forever
	handlersOf(c).setReq(func(r ServerRequest) {
		close(handlerStarted)
		<-neverRelease
	})
	c.dispatchServerRequest(ServerRequest{ID: json.RawMessage(`"srv-1"`), Method: "m"})
	<-handlerStarted // handler executing and wedged

	start := time.Now()
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	// Upper bound: the budget plus generous scheduling headroom — Close
	// must NEVER wait for the never-releasing handler itself.
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 10*budget {
			t.Fatalf("Close took %v — exceeded its %v bound (L1 regression)", elapsed, budget)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung on a wedged reqWorker handler — teardown is unbounded (L1 regression)")
	}
	// The worker is still wedged (expected); do not release — the test ends
	// and the process exits; the goroutine leak is confined to the test.
}
