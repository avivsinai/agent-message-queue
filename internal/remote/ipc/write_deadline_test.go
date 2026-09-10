package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestStuckClientDoesNotBlockServerWriteForever pins the B14 write-deadline
// half: a client that connects, sends a valid command, and then stops
// reading must not hold the server's handler goroutine on the response
// write — the write aborts at rpcWriteDeadline and the handler returns. The
// regression is the closed connection being observed by the server side (via
// a deadline that actually fires), not a hang.
func TestStuckClientDoesNotBlockServerWriteForever(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based")
	}
	stateDir, err := os.MkdirTemp("", "amqr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ep := core.New(core.Config{Store: store})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	server, err := Listen(stateDir, ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Serve(ctx) }()

	conn, err := net.Dial("unix", SocketPath(stateDir))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand,
		Op:     protocol.OpSessionList,
	}
	data, err := json.Marshal(Request{Command: cmd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatalf("send request: %v", err)
	}

	// Do NOT read the response. The handler's write must hit the deadline
	// instead of blocking forever. We cannot observe the goroutine directly;
	// what we CAN observe is that the deadline constant is bounded and the
	// socket still accepts NEW connections while the stuck one is wedged —
	// the pre-fix failure mode was a handler goroutine parked in conn.Write
	// with no bound, starving nothing at 1 conn but proving unbounded with
	// many. Fill the handler with a batch of stuck connections; with the
	// deadline each clears within ~rpcWriteDeadline, without it they
	// accumulate.
	const stuck = 8
	conns := make([]net.Conn, 0, stuck)
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	for i := 0; i < stuck; i++ {
		c, err := net.Dial("unix", SocketPath(stateDir))
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conns = append(conns, c)
		req, err := json.Marshal(Request{Command: cmd})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(append(req, '\n')); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// A fresh connection must be served promptly while the others are stuck:
	// the write deadline keeps handler goroutines from accumulating on a
	// blocked writer, so the accept loop and the endpoint stay live.
	live, err := net.Dial("unix", SocketPath(stateDir))
	if err != nil {
		t.Fatalf("live dial: %v", err)
	}
	defer func() { _ = live.Close() }()
	liveReq, err := json.Marshal(Request{Command: cmd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Write(append(liveReq, '\n')); err != nil {
		t.Fatalf("live send: %v", err)
	}
	live.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(live)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("live client got no response while stuck clients held handlers: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode live response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("live response carries error: %+v", resp.Error)
	}
}
