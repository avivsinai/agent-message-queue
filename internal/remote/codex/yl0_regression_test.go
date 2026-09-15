package codex

import (
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression for agent-message-queue-yl0 (and the Inspect half of
// agent-message-queue-611.22.51): Inspect() picked PendingInteraction by
// ranging over the a.runs MAP and keeping the last interaction found. Go
// randomizes map iteration order, so with two runs both carrying an
// interaction the published Session.PendingInteraction flapped between
// calls with no state change.
//
// Fix rule (product decision per the bead): the operator should respond to
// the interaction that has been waiting the LONGEST — the oldest
// interaction by run createdAt (ties broken by run key for full
// determinism). A codex thread is a single conversation; the first
// approval/question the harness raised is the one blocking its turn, and
// every later interaction sits behind it.
func TestYl0PendingInteractionIsDeterministicOldest(t *testing.T) {
	// 611.22.33 test-bar: a step clock replaces the 5ms sleeps between the
	// two submits. The old sleeps existed only to make run.createdAt
	// strictly monotonic — wall-clock dependent, and a coarsely-ticked
	// clock (or a loaded runner) could give both runs the same createdAt,
	// silently changing the tie-break path the test exercises. With a
	// stepped fake clock every run gets a strictly later timestamp
	// deterministically, and the tie-break branch below is still
	// exercised by the explicit createdAt tie-break assertion.
	step := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	clockMu := new(sync.Mutex)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		step = step.Add(time.Second)
		return step
	}
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(200*time.Millisecond), WithApprovals(true), WithClock(clock))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	epoch := s.Epoch

	// Two runs, each with a pending interaction. The FIRST run submitted is
	// the one whose interaction must always be reported, regardless of map
	// iteration order.
	keys := []requests.Key{
		{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-1111111110l0"},
		{CreatorHost: "local", TargetID: s.TargetID, RequestID: "22222222-2222-4222-8222-2222222220l0"},
	}
	for i, key := range keys {
		admCh := make(chan error, 1)
		go func(k requests.Key) {
			_, err := att.Submit(core.BoundRequest{Key: k, Epoch: epoch, Input: protocol.SubmitInput{Text: "work " + k.RequestID, Busy: protocol.BusyQueue}})
			admCh <- err
		}(key)
		// The first submit takes the turn/start path (idle thread); the
		// second sees a busy thread and takes thread/queue/add. Both arrive
		// on srv.calls — drain one RPC per iteration regardless of which.
		select {
		case call := <-srv.calls:
			if call.Method != "turn/start" && call.Method != "thread/queue/add" {
				t.Fatalf("submit %d: unexpected RPC %s", i, call.Method)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no RPC for submit %d ever arrived", i)
		}
		select {
		case <-admCh:
		case <-time.After(10 * time.Second):
			t.Fatalf("submit %d did not return", i)
		}
		// Confirm the run via its own userMessage AFTER Submit has
		// registered it: interactions attach through byTurn, which is
		// populated ONLY by the confirming item/started carrying the
		// clientId (verifier round-1 blocker — without this the handler
		// returns early and the test self-skips). Sending it before the
		// run exists races the registration.
		srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"turnYl0`+string(rune('A'+i))+`"}}`)
		srv.notify(t, "item/started", `{"threadId":"t1","turnId":"turnYl0`+string(rune('A'+i))+`","item":{"type":"userMessage","id":"i`+string(rune('A'+i))+`","clientId":"`+clientIDFor(key)+`","content":[]}}`)
		// The server request registers the interaction on the (now bound)
		// run; waitInteraction-style bounded poll, no sleep-assertion.
		srv.sendServerRequest(t, "srvReq"+string(rune('A'+i)), "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"turnYl0`+string(rune('A'+i))+`","itemId":"tool`+string(rune('A'+i))+`","command":{"prompt":"approve?"},"availableDecisions":["accept","decline"]}`)
		deadline := time.Now().Add(5 * time.Second)
		registered := false
		for time.Now().Before(deadline) {
			att.mu.Lock()
			r, ok := att.runs[key]
			set := ok && r.interaction != nil
			att.mu.Unlock()
			if set {
				registered = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !registered {
			t.Fatalf("interaction %d never registered (byTurn not bound — setup bug, 7xl/yl0)", i)
		}
		// createdAt ordering is now guaranteed by the stepped fake clock —
		// no wall-clock sleep needed.
	}

	att.mu.Lock()
	pending := 0
	for _, r := range att.runs {
		if r.interaction != nil {
			pending++
		}
	}
	att.mu.Unlock()
	// 7xl/yl0 rule: a regression test that disables itself on setup failure
	// is a defect, not a guard — fail loudly instead of skipping (verifier
	// round-1: the old Skipf made both assertions dead code, 0/20 runs ever
	// reached them).
	if pending < 2 {
		t.Fatalf("setup: only %d pending interactions registered, want 2", pending)
	}

	// The published PendingInteraction must be the OLDEST interaction's id,
	// stably, across many Inspect calls (map order randomizes per call).
	var first *string
	for i := 0; i < 50; i++ {
		insp := att.Inspect()
		got := insp.PendingInteraction
		if got == nil {
			t.Fatalf("inspect %d: PendingInteraction nil with %d pending", i, pending)
		}
		if first == nil {
			first = got
		} else if *first != *got {
			t.Fatalf("PendingInteraction flapped: %s vs %s (map-order pick, yl0)", *first, *got)
		}
	}

	// And it must be the oldest run's interaction id, not an arbitrary one.
	att.mu.Lock()
	var oldest string
	var oldestAt time.Time
	for _, r := range att.runs {
		if r.interaction == nil {
			continue
		}
		if oldest == "" || r.createdAt.Before(oldestAt) || (r.createdAt.Equal(oldestAt) && r.interaction.InteractionID < oldest) {
			oldestAt = r.createdAt
			oldest = r.interaction.InteractionID
		}
	}
	att.mu.Unlock()
	if oldest == "" {
		t.Fatal("no pending interaction found in runs")
	}
	if *first != oldest {
		t.Fatalf("published %s, want oldest interaction %s (yl0)", *first, oldest)
	}
}
