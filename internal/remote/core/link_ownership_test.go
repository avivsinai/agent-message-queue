package core_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// A linked server sees and touches only what it created and the targets
// shared with it, and cannot fill the machine's store
// (agent-message-queue-9dx.3, design 4.2 §7 changes 4 and 5).

const (
	hostA = "link-aaaaaaaaaaaaaaaa"
	hostB = "link-bbbbbbbbbbbbbbbb"
)

func linkSource(host string, shared ...string) core.Source {
	return core.Source{
		Host:   host,
		Origin: map[string]string{"carrier": core.CarrierLink, core.OriginSink: host},
		Shared: append([]string{}, shared...),
	}
}

var mailboxSource = core.Source{Host: "amq:peer", Origin: map[string]string{"carrier": "amq"}}

// linkClock is a settable clock: the busy test moves past the compaction
// horizon.
type linkClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *linkClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *linkClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newLinkEndpoint(t *testing.T, atts ...core.Attachment) (*core.Endpoint, *requests.Store, *linkClock) {
	t.Helper()
	clk := &linkClock{t: time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)}
	store, err := requests.Open(t.TempDir(), requests.WithClock(clk.now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ep := core.New(core.Config{Store: store, Now: clk.now, CompactHorizon: protocol.DefaultCompactHorizon,
		Consent: func(string, string) bool { return true }})
	for _, a := range atts {
		ep.Register(a)
	}
	t.Cleanup(func() { _ = ep.Close() })
	return ep, store, clk
}

func linkSubmit(clk *linkClock, target, id string) *protocol.Command {
	return &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: target, Epoch: "e_1",
		NotAfter: protocol.FormatTime(clk.now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "run the tests"},
	}
}

func linkCancel(clk *linkClock, ref, target string) *protocol.Command {
	return &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestCancel,
		RequestRef: ref, TargetID: target, Epoch: "e_1",
		NotAfter: protocol.FormatTime(clk.now().Add(2 * time.Minute)),
	}
}

func getCmd(ref string) *protocol.Command {
	return &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}
}

// submitReply submits as a verified signed submit does: a link source carries
// the native session it signed for (the fake's is its target id) and the
// consent key that signed.
func submitReply(t *testing.T, ep *core.Endpoint, cmd *protocol.Command, src core.Source) protocol.Reply {
	t.Helper()
	if src.Origin["carrier"] == core.CarrierLink {
		src.NativeSession, src.Credential = cmd.TargetID, "cred-1"
	}
	out, err := ep.Handle(cmd, src)
	if err != nil {
		t.Fatalf("submit %s: %v", cmd.RequestID, err)
	}
	return out.(protocol.Reply)
}

func refusalCode(err error) protocol.Code {
	var r *protocol.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func TestLinkReadsAndCancelsOnlyItsOwnRequests(t *testing.T) {
	ep, store, clk := newLinkEndpoint(t, fake.New("fake", "e_1"))
	const idA = "11111111-1111-4111-8111-1111111111a1"
	reply := submitReply(t, ep, linkSubmit(clk, "fake", idA), linkSource(hostA, "fake"))
	refA := reply.Snapshot.RequestRef

	if _, err := ep.Handle(getCmd(refA), linkSource(hostA, "fake")); err != nil {
		t.Fatalf("link A get of its own request: %v", err)
	}
	cases := []struct {
		name string
		cmd  *protocol.Command
		src  core.Source
		want protocol.Code
	}{
		{"link B get", getCmd(refA), linkSource(hostB, "fake"), protocol.CodeNotFound},
		{"link B cancel", linkCancel(clk, refA, "fake"), linkSource(hostB, "fake"), protocol.CodeNotFound},
		{"mailbox get", getCmd(refA), mailboxSource, protocol.CodeUnshared},
		{"link B cancel of an absent id", linkCancel(clk, protocol.EncodeRef(hostB, "fake", "11111111-1111-4111-8111-1111111111b9"), "fake"), linkSource(hostB, "fake"), protocol.CodeNotFound},
	}
	for _, tc := range cases {
		if _, err := ep.Handle(tc.cmd, tc.src); refusalCode(err) != tc.want {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.want)
		}
	}
	recs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].State != protocol.StateRunning {
		t.Fatalf("records = %d (first %+v), want only A's running request: a link cancel left a record", len(recs), recs)
	}
}

func TestLinkBusyEndsRefusedCompactsAndIsNeverReadmitted(t *testing.T) {
	rt := fake.New("fake", "e_1")
	ep, store, clk := newLinkEndpoint(t, rt)
	const idA, idB = "11111111-1111-4111-8111-1111111111a2", "11111111-1111-4111-8111-1111111111b2"
	submitReply(t, ep, linkSubmit(clk, "fake", idA), linkSource(hostA, "fake"))

	busy := submitReply(t, ep, linkSubmit(clk, "fake", idB), linkSource(hostB, "fake"))
	keyB := requests.Key{CreatorHost: hostB, TargetID: "fake", RequestID: idB}
	rec, _, err := store.Get(keyB)
	if err != nil || rec == nil {
		t.Fatalf("busy link request left no record: %v", err)
	}
	if busy.Outcome.Code != protocol.CodeBusy || rec.State != protocol.StateRejected || rec.Code != protocol.CodeBusy || rec.Tombstone {
		t.Fatalf("busy link submit = outcome %q, record %s/%s tombstone=%v; want a refused busy record, no tombstone",
			busy.Outcome.Code, rec.State, rec.Code, rec.Tombstone)
	}

	rt.Complete(idA, "done")
	clk.advance(protocol.DefaultCompactHorizon + time.Minute)
	if err := ep.Tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec, _, _ = store.Get(keyB); !rec.Tombstone {
		t.Fatalf("refused busy link record not compacted after the horizon: %+v", rec)
	}

	// The target is free now; the same request id must still not run.
	if n := rt.UnacknowledgedResults(); n != 0 {
		t.Fatalf("fake still holds %d unacknowledged results", n)
	}
	before := rt.Snapshot().Dispatches
	again := submitReply(t, ep, linkSubmit(clk, "fake", idB), linkSource(hostB, "fake"))
	if again.Snapshot.State != protocol.StateRejected || rt.Snapshot().Dispatches != before || rt.HasRun(idB) {
		t.Fatalf("resubmit of compacted busy link request = %s, dispatches %d -> %d: re-admitted",
			again.Snapshot.State, before, rt.Snapshot().Dispatches)
	}
}

// activeSession reports an active request and a pending interaction, as a
// harness running someone else's request does.
type activeSession struct {
	*fake.Runtime
	ref, interaction string
}

func (a activeSession) Inspect() protocol.Session {
	s := a.Runtime.Inspect()
	s.ActiveRequestRef, s.PendingInteraction = &a.ref, &a.interaction
	return s
}

func TestLinkSessionListShowsOnlySharedTargets(t *testing.T) {
	shared := activeSession{fake.New("shared", "e_1"), protocol.EncodeRef("local", "shared", "11111111-1111-4111-8111-1111111111c1"), "int_1"}
	ep, _, _ := newLinkEndpoint(t, shared, fake.New("private", "e_1"))

	out, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionList}, linkSource(hostA, "shared"))
	if err != nil {
		t.Fatalf("session.list: %v", err)
	}
	sessions := out.([]protocol.Session)
	if len(sessions) != 1 || sessions[0].TargetID != "shared" {
		t.Fatalf("session.list from a link = %+v, want only the shared target", sessions)
	}
	s := sessions[0]
	if s.ActiveRequestRef != nil || s.PendingInteraction != nil || s.Capabilities.ApproveTool || s.Capabilities.AnswerQuestion {
		t.Fatalf("shared session shows another source's request or answering capabilities: %+v", s)
	}
	_, err = ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: "private"}, linkSource(hostA, "shared"))
	if refusalCode(err) != protocol.CodeNotFound {
		t.Fatalf("inspect of an unshared target = %v, want not_found", err)
	}
}

func TestLinkHoldsAtMostFourOpenRequests(t *testing.T) {
	targets := []string{"t1", "t2", "t3", "t4", "t5"}
	atts := make([]core.Attachment, len(targets))
	rts := make([]*fake.Runtime, len(targets))
	for i, id := range targets {
		rts[i] = fake.New(id, "e_1")
		atts[i] = rts[i]
	}
	ep, store, clk := newLinkEndpoint(t, atts...)
	for i, id := range targets[:4] {
		if r := submitReply(t, ep, linkSubmit(clk, id, fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)), linkSource(hostA, targets...)); r.Snapshot.State != protocol.StateRunning {
			t.Fatalf("submit %d = %s, want running", i+1, r.Snapshot.State)
		}
	}
	fifth := submitReply(t, ep, linkSubmit(clk, "t5", "11111111-1111-4111-8111-111111111105"), linkSource(hostA, targets...))
	if fifth.Outcome.Code != protocol.CodeBusy || rts[4].Snapshot().Dispatches != 0 {
		t.Fatalf("fifth open link submit = %q with %d dispatches, want busy before any dispatch", fifth.Outcome.Code, rts[4].Snapshot().Dispatches)
	}
	if recs, _ := store.List(); len(recs) != 4 {
		t.Fatalf("records = %d, want 4: the refused fifth submit left a record", len(recs))
	}
	if r := submitReply(t, ep, linkSubmit(clk, "t5", "11111111-1111-4111-8111-1111111111b5"), linkSource(hostB, "t5")); r.Snapshot.State != protocol.StateRunning {
		t.Fatalf("another link's submit = %s, want running: the bound is per link", r.Snapshot.State)
	}
}
