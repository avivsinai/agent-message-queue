package core_test

import (
	"context"
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
// (agent-message-queue-9dx.3).

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
	store, err := requests.Open(t.TempDir())
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
		{"local cancel of an absent link id", linkCancel(clk, protocol.EncodeRef(hostB, "fake", "11111111-1111-4111-8111-1111111111b8"), "fake"), core.Source{Host: core.LocalHost}, protocol.CodeUnshared},
	}
	for _, tc := range cases {
		if _, err := ep.Handle(tc.cmd, tc.src); refusalCode(err) != tc.want {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.want)
		}
	}
	if r := submitReply(t, ep, linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111b7"), linkSource(hostB)); r.Outcome.Code != protocol.CodeUnshared {
		t.Errorf("link submit to an unshared target = %q, want unshared", r.Outcome.Code)
	}
	// The user's own terminal reads every request, a link's included.
	if _, err := ep.WaitAfter(context.Background(), refA, new(int64)); err != nil {
		t.Errorf("local wait on a link request: %v", err)
	}
	if _, err := ep.Handle(getCmd(refA), core.Source{Host: core.LocalHost}); err != nil {
		t.Errorf("local get of a link request: %v", err)
	}
	recs, _, err := store.ListWithPoison()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].State != protocol.StateRunning {
		t.Fatalf("records = %d (first %+v), want only A's running request: a link cancel left a record", len(recs), recs)
	}
}

// The contract changed by design: a link's busy refusal was a terminal record,
// so sending again needed a new request id and a second passkey signature. It
// is now re-admitted only by the same signed request (id, input and
// not_after) before its deadline, and runs at most once.
func TestLinkBusyIsReadmittedOnlyByTheSameSignedRequest(t *testing.T) {
	rt := fake.New("fake", "e_1")
	ep, store, clk := newLinkEndpoint(t, rt)
	const idA = "11111111-1111-4111-8111-1111111111a2"
	submitReply(t, ep, linkSubmit(clk, "fake", idA), linkSource(hostA, "fake"))

	// Three busy requests of B while A runs; each is a busy tombstone with no
	// input. Each command is kept: sending it again is the same signed
	// document.
	runs := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111b2")
	expires := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111b3")
	resigned := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111b4")
	keyOf := func(cmd *protocol.Command) requests.Key {
		return requests.Key{CreatorHost: hostB, TargetID: "fake", RequestID: cmd.RequestID}
	}
	for _, cmd := range []*protocol.Command{runs, expires, resigned} {
		busy := submitReply(t, ep, cmd, linkSource(hostB, "fake"))
		rec, _, err := store.Get(keyOf(cmd))
		if err != nil || rec == nil {
			t.Fatalf("busy link request left no record: %v", err)
		}
		if busy.Outcome.Code != protocol.CodeBusy || rec.State != protocol.StateRejected || rec.Code != protocol.CodeBusy || !rec.Tombstone || rec.Input != nil {
			t.Fatalf("busy link submit = outcome %q, record %s/%s tombstone=%v input=%v; want a busy tombstone with no input",
				busy.Outcome.Code, rec.State, rec.Code, rec.Tombstone, rec.Input)
		}
	}
	// A busy tombstone of a source that is not a link, under the link's host.
	local := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111b5")
	if r := submitReply(t, ep, local, core.Source{Host: hostB}); r.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("non-link submit while busy = %q, want busy", r.Outcome.Code)
	}
	rt.Complete(idA, "done")
	dispatches := func() int { return rt.Snapshot().Dispatches }

	// Another signature for the same id (a later not_after) does not run: it
	// is answered from the stored record. Review of this branch: the outcome
	// had no code, which a link server reads as accepted; it is
	// request_conflict.
	later := *resigned
	later.NotAfter = protocol.FormatTime(clk.now().Add(3 * time.Minute))
	before := dispatches()
	if r := submitReply(t, ep, &later, linkSource(hostB, "fake")); r.Snapshot.State != protocol.StateRejected || r.Snapshot.Code != protocol.CodeBusy || r.Outcome.Code != protocol.CodeRequestConflict || dispatches() != before || rt.HasRun(resigned.RequestID) {
		t.Fatalf("resubmit with a later not_after = %s/%s outcome %q, dispatches %d -> %d; want the stored busy record, request_conflict, no run",
			r.Snapshot.State, r.Snapshot.Code, r.Outcome.Code, before, dispatches())
	}

	// Review of this branch: a source that is not a link, with the link's
	// host, ran the link's tombstone without signed bytes. Only a link
	// re-admits it.
	if _, err := ep.Handle(runs, core.Source{Host: hostB}); refusalCode(err) != protocol.CodeConsentInvalid || dispatches() != before || rt.HasRun(runs.RequestID) {
		t.Fatalf("non-link resubmit of a link tombstone = %v, dispatches %d -> %d; want consent_invalid, no run", err, before, dispatches())
	}

	// Review of this branch: a signed link submit with the same id and input
	// ran the busy tombstone of a source that is not a link. Only a source of
	// the record's own kind re-admits it.
	signed := linkSource(hostB, "fake")
	signed.NativeSession, signed.Credential = "fake", "cred-1"
	if _, err := ep.Handle(local, signed); refusalCode(err) != protocol.CodeConsentInvalid || dispatches() != before || rt.HasRun(local.RequestID) {
		t.Fatalf("signed link resubmit of a non-link tombstone = %v, dispatches %d -> %d; want consent_invalid, no run", err, before, dispatches())
	}

	// The same signed document runs once the target is free, and only once.
	if r := submitReply(t, ep, runs, linkSource(hostB, "fake")); r.Snapshot.State != protocol.StateRunning || dispatches() != before+1 || !rt.HasRun(runs.RequestID) {
		t.Fatalf("same signed resubmit = %s %q, dispatches %d -> %d; want it to run", r.Snapshot.State, r.Outcome.Code, before, dispatches())
	}
	submitReply(t, ep, runs, linkSource(hostB, "fake"))
	if dispatches() != before+1 {
		t.Fatalf("a third identical submit dispatched again: %d -> %d", before, dispatches())
	}
	// Review of this branch: after the request ran, another signature for
	// its id (a later not_after) was answered with no code, which a link
	// server reads as accepted. It is request_conflict in every state.
	ranLater := *runs
	ranLater.NotAfter = protocol.FormatTime(clk.now().Add(3 * time.Minute))
	if r := submitReply(t, ep, &ranLater, linkSource(hostB, "fake")); r.Outcome.Code != protocol.CodeRequestConflict || dispatches() != before+1 {
		t.Fatalf("a later not_after for a request that ran = outcome %q, dispatches %d -> %d; want request_conflict, no run", r.Outcome.Code, before, dispatches())
	}

	// Past its not_after the same document is refused expired, and after the
	// compaction horizon it still never runs.
	rt.Complete(runs.RequestID, "done")
	clk.advance(3 * time.Minute)
	before = dispatches()
	if r := submitReply(t, ep, expires, linkSource(hostB, "fake")); r.Outcome.Code != protocol.CodeExpired || dispatches() != before || rt.HasRun(expires.RequestID) {
		t.Fatalf("resubmit past not_after = %q, dispatches %d -> %d; want expired, no run", r.Outcome.Code, before, dispatches())
	}
	clk.advance(protocol.DefaultCompactHorizon + time.Minute)
	if err := ep.Tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec, _, _ := store.Get(keyOf(expires)); rec == nil || !rec.Tombstone || rec.Input != nil {
		t.Fatalf("busy link record after the horizon = %+v, want a tombstone with no input", rec)
	}
	if r := submitReply(t, ep, expires, linkSource(hostB, "fake")); r.Snapshot.State != protocol.StateRejected || dispatches() != before || rt.HasRun(expires.RequestID) {
		t.Fatalf("resubmit after the horizon = %s, dispatches %d -> %d; want no run", r.Snapshot.State, before, dispatches())
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
	if recs, _, _ := store.ListWithPoison(); len(recs) != 4 {
		t.Fatalf("records = %d, want 4: the refused fifth submit left a record", len(recs))
	}
	if r := submitReply(t, ep, linkSubmit(clk, "t5", "11111111-1111-4111-8111-1111111111b5"), linkSource(hostB, "t5")); r.Snapshot.State != protocol.StateRunning {
		t.Fatalf("another link's submit = %s, want running: the bound is per link", r.Snapshot.State)
	}
}
