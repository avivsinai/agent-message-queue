package codex

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// b14bDeadline bounds every blocking wait: a regression FAILS, never hangs CI.
const b14bDeadline = 5 * time.Second

// b14bWait polls cond until true or the deadline passes.
func b14bWait(cond func() bool) bool {
	deadline := time.Now().Add(b14bDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

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
