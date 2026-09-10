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
	c := newClient(ws)
	if onNotif != nil {
		c.OnNotification = func(n Notification) { onNotif() }
	}
	if onReq != nil {
		c.OnServerRequest = onReq
	}
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
		payload, err := ws.readText()
		if err != nil {
			return
		}
		onFrame(payload)
		// Keep draining so further writes never wedge the client.
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
	c := newClient(ws)
	t.Cleanup(func() { _ = c.Close() })
	return c, l
}

// sendNotif / sendReq let tests inject reader-side frames as if the server
// sent them — but the real readLoop is the only writer-safe reader of the
// socket, so tests instead deliver via a direct server frame over the conn.
// Simplest deterministic injection: call the dispatch paths the readLoop
// would call, since B9/B6/B8 target those paths' lifecycle, not the parse.

// TestB14bCloseRunsWorkerStopAndWorkerExits pins B9: Close must run the
// STOP lifecycle (previously the shared sync.Once meant close(events) never
// ran and the worker leaked on every teardown). The worker closes
// workerDone on exit; the test waits on it with a deadline.
func TestB14bCloseRunsWorkerStopAndWorkerExits(t *testing.T) {
	processed := make(chan struct{})
	c, _ := newB14bClient(t, nil, nil)
	c.OnNotification = func(n Notification) { close(processed) }

	// Queue a callback so the worker is observably running, then Close.
	c.dispatch(func() { c.OnNotification(Notification{Method: "x"}) })
	select {
	case <-processed:
	case <-time.After(b14bDeadline):
		t.Fatal("worker never processed the callback")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Worker must exit: workerDone closed. Regression (leaked worker) FAILS
	// here by deadline instead of hanging.
	select {
	case <-c.workerDone:
	case <-time.After(b14bDeadline):
		t.Fatal("worker did not exit after Close — close(events) never ran (B9 regression)")
	}
}

// TestB14bWaitCallbacksCountsInFlight pins B9's second half: waitCallbacks
// must await the EXECUTING callback (len(events) excludes it). A callback
// that blocks is awaited until the deadline, then Close returns anyway.
func TestB14bWaitCallbacksCountsInFlight(t *testing.T) {
	release := make(chan struct{})
	handlerRunning := make(chan struct{})
	c, _ := newB14bClient(t, nil, nil)
	c.OnNotification = func(n Notification) {
		close(handlerRunning)
		<-release
	}
	c.dispatch(func() { c.OnNotification(Notification{Method: "x"}) })
	<-handlerRunning
	done := make(chan struct{})
	go func() { c.waitCallbacks(); close(done) }()
	// waitCallbacks must NOT return while the callback is in flight.
	select {
	case <-done:
		t.Fatal("waitCallbacks returned while callback still executing (in-flight not tracked)")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(b14bDeadline):
		t.Fatal("waitCallbacks never returned after in-flight callback finished")
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
	c := newClient(ws)
	t.Cleanup(func() { _ = c.Close() })

	// Writer A: a large payload wedges in conn.Write (buffer full).
	writeADone := make(chan error, 1)
	go func() {
		big := make([]byte, 512*1024) // exceeds any socket buffer
		writeADone <- ws.writeText(big)
	}()
	// Give A time to acquire the slot and block inside Write.
	time.Sleep(150 * time.Millisecond)

	// Call B: small payload, 250ms deadline. The slot is held by A's
	// writeText (uncontested under the old mutex: B waited unboundedly).
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
	_ = writeADone // A stays wedged; the test ends and cleanup closes everything.
}

// TestB14bReplyRequiredNotDroppedAndPumpStaysLive pins B8: notifications may
// drop on a full backlog, but reply-required server requests must never be
// silently dropped — delivered via their own queue, or explicitly failed on
// the should-be-impossible overflow — and the read pump never blocks on
// handler latency.
func TestB14bReplyRequiredNotDroppedAndPumpStaysLive(t *testing.T) {
	var mu sync.Mutex
	notifications := 0
	approved := make(chan string, 8)
	blockApproval := make(chan struct{}) // wedges the first approval handler
	c, _ := newB14bClient(t, nil, nil)
	c.OnNotification = func(n Notification) { mu.Lock(); notifications++; mu.Unlock() }
	first := true
	c.OnServerRequest = func(r ServerRequest) {
		mu.Lock()
		approved <- r.Method
		mu.Unlock()
		if first {
			first = false
			<-blockApproval // wedge AFTER recording: the pump must survive it
		}
	}

	// Flood notifications (bounded queue of 64): drops are legal. Then a
	// reply-required request: it MUST be delivered (own queue) even with the
	// notification backlog full.
	for i := 0; i < 100; i++ {
		c.dispatch(func() { c.OnNotification(Notification{Method: "noise"}) })
	}
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
	c2.OnServerRequest = func(r ServerRequest) {
		// Signal that the req worker has consumed the head from reqQ, then
		// wedge: the queue below can now fill deterministically.
		close(headPicked)
		<-release2
	}
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
	// Pump liveness while an approval handler was wedged: notifications kept
	// flowing (delivered or dropped — the readLoop never stalled).
	mu.Lock()
	n := notifications
	mu.Unlock()
	if n == 0 && len(c.events) == 0 {
		t.Fatal("notification worker never made progress while approval handler wedged")
	}
}
