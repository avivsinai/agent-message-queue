package turn

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// remote is a turn's pin to a real endpoint: its state directory, native
// session pin and durations.
type remote struct {
	dir    string
	native string
	timing Timing
}

// remoteServer runs a real endpoint socket with att and returns the pin of
// target "fake" and native session "fake" behind it.
func remoteServer(t *testing.T, att core.Attachment, crash core.CrashPoint) *remote {
	t.Helper()
	dir := filepath.Join(endpointRoot(t), "extensions", "remote")
	store, err := requests.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store, Crash: crash})
	ep.Register(att)
	srv, err := ipc.Listen(dir, ep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = ep.Close(); _ = store.Close() })
	return &remote{dir: dir, native: "fake", timing: Timing{Timeout: time.Second, Poll: 100 * time.Millisecond, Heartbeat: 10 * time.Millisecond}}
}

// endpointRoot is a fresh AMQ root under the caller's TMPDIR, named short so
// the endpoint socket beneath it fits the Unix socket path limit (104 bytes
// on macOS). A TMPDIR too long for it fails setup and names the limit.
func endpointRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if sock := ipc.SocketPath(filepath.Join(root, "extensions", "remote")); runtime.GOOS != "windows" && len(sock) >= 104 {
		t.Fatalf("endpoint socket %s is %d bytes, over the 104-byte Unix socket path limit; set a shorter TMPDIR", sock, len(sock))
	}
	return root
}

// run runs text as one turn of eventID against the endpoint, with emit as
// its client (nil: no client).
func (e *remote) run(text, eventID string, turn *state, emit func(update, text string) error) (Result, error) {
	r := &Turn{Name: "amq-acp", Dir: e.dir, Target: "fake", Native: e.native, EventID: eventID, Timing: e.timing, Done: turn.done, Settle: turn.settle, Emit: emit}
	if emit != nil {
		r.Mirror = func(json.RawMessage) error { return nil }
	}
	return r.Run(text)
}

// state is a turn's outcome as its caller keeps it: decided once, under a
// lock.
type state struct {
	mu      sync.Mutex
	outcome Outcome
	done    chan struct{}
}

func newTurn() *state { return &state{done: make(chan struct{})} }

func (s *state) settle(outcome Outcome) Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if outcome != Open && s.outcome == Open {
		s.outcome = outcome
	}
	return s.outcome
}

// stop settles the turn from outside, as a client's cancel does.
func (s *state) stop(outcome Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcome == Open {
		s.outcome = outcome
		close(s.done)
	}
}

type noCancel struct{ *fake.Runtime }

func (r noCancel) CancelExact(requests.Key, string) (core.CancelEvidence, error) {
	return core.CancelEvidence{Disposition: protocol.CancelUnsupported, Message: "native cancel unavailable"}, nil
}

// Regression: #876, an unsupported cancel was reported as a plain cancelled
// turn while the native work kept running.
func TestRemoteUnsupportedCancelSaysWorkContinues(t *testing.T) {
	rt := fake.New("fake", "e_1")
	e := remoteServer(t, noCancel{rt}, nil)
	turn := newTurn()
	var said []string
	got, err := e.run("hello", strings.Repeat("a", 64), turn, func(update, text string) error {
		if update == "agent_message_chunk" {
			said = append(said, text)
		}
		turn.stop(Cancelled)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.Remote.Cancel != string(protocol.CancelUnsupported) || len(said) != 1 || !strings.Contains(said[0], "work continues") {
		t.Fatalf("result=%+v said=%q", got, said)
	}
}

// Regression: #876, an error after the native run was reported as
// not_submitted with no request reference.
func TestRemoteErrorAfterNativeRunKeepsRequest(t *testing.T) {
	rt := fake.New("fake", "e_1")
	e := remoteServer(t, rt, func(point string) error {
		if point == core.PointAfterNative {
			return core.ErrCrashed
		}
		return nil
	})
	result, err := e.run("hello", strings.Repeat("b", 64), newTurn(), func(string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	got := result.Meta.Remote
	if got.State == StateNotSubmitted || got.RequestRef == "" {
		t.Fatalf("native run exists but result is %+v", got)
	}
}

type movingNative struct {
	*fake.Runtime
	id string
}

func (m *movingNative) NativeSessionID() string { return m.id }

// Regression: #876, a replacement native session behind the same target
// received the owner's prompts.
func TestRemoteRefusesReplacementNativeSession(t *testing.T) {
	rt := fake.New("fake", "e_1")
	e := remoteServer(t, &movingNative{Runtime: rt, id: "replacement"}, nil)
	got, err := e.run("hello", "", newTurn(), func(string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.Remote.Code != string(protocol.CodeUnshared) || len(rt.Snapshot().RunningKeys) != 0 {
		t.Fatalf("replacement session was not refused: %+v", got)
	}
}

// Regression: #876, a redelivered event after the endpoint reattached under a
// new epoch got request_conflict instead of its stored result.
func TestRemoteRedeliveryAfterReattachReturnsStoredResult(t *testing.T) {
	rt := fake.New("fake", "e_1")
	e := remoteServer(t, rt, nil)
	eventID := strings.Repeat("c", 64)
	id, _ := RequestID(eventID)
	run := func(emit func(update, text string) error) Result {
		t.Helper()
		result, err := e.run("hello", eventID, newTurn(), emit)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if first := run(func(string, string) error { rt.Complete(id, "native result"); return nil }); first.StopReason != StopReasonEndTurn {
		t.Fatalf("first = %+v", first)
	}
	rt.SwitchSession("e_2")
	if second := run(func(string, string) error { return nil }); second.StopReason != StopReasonEndTurn || second.Meta.Remote.State != "completed" {
		t.Fatalf("redelivery = %+v", second)
	}
}

// Regression: #876, a cancel that settled first was overwritten by a
// completed snapshot.
func TestRemoteSettledCancelWins(t *testing.T) {
	turn := &state{done: make(chan struct{}), outcome: "session_cancelled"}
	r := &Turn{Emit: func(string, string) error { return nil }, Done: turn.done, Settle: turn.settle}
	close(turn.done)
	got, err := r.follow(protocol.Snapshot{State: protocol.StateCompleted, Result: &protocol.Result{Text: "done"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != StopReasonCancelled {
		t.Fatalf("settled cancel overwritten: %+v", got)
	}
}

// Regression: #876, a redelivery whose lookup failed asserted
// not_submitted while the original native run existed.
func TestRemoteRedeliveryLookupFailureIsUncertain(t *testing.T) {
	rt := fake.New("fake", "e_1")
	e := remoteServer(t, rt, nil)
	eventID := strings.Repeat("e", 64)
	if _, err := e.run("hello", eventID, newTurn(), func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	path := ipc.SocketPath(e.dir)
	if err := os.Rename(path, path+".hold"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(path+".hold", path) })
	result, err := e.run("hello", eventID, newTurn(), func(string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Meta.Remote; got.State != StateUncertain || got.RequestRef == "" {
		t.Fatalf("redelivery = %+v", got)
	}
}

// Regression: #876, the replay path read a completed request with a wrong
// native pin.
func TestRemoteReplayWithWrongPinIsRefused(t *testing.T) {
	rt := fake.New("fake", "e_1")
	e := remoteServer(t, rt, nil)
	eventID := strings.Repeat("f", 64)
	id, _ := RequestID(eventID)
	if _, err := e.run("hello", eventID, newTurn(), func(string, string) error { rt.Complete(id, "private result"); return nil }); err != nil {
		t.Fatal(err)
	}
	e.native = "another-session"
	got, err := e.run("hello", eventID, newTurn(), func(string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason == StopReasonEndTurn || got.Meta.Remote.Code != string(protocol.CodeUnshared) {
		t.Fatalf("wrong pin read the request: %+v", got)
	}
}

// Regression: #876, a busy-rejected event could not be retried after the
// target became free.
func TestRemoteBusyRedeliveryRetries(t *testing.T) {
	rt := fake.New("fake", "e_1")
	e := remoteServer(t, rt, nil)
	busyID := "d2c80e1d-feb7-4c10-959e-23456789abcd"
	resp, err := ipc.Call(e.dir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: busyID, TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)), Input: &protocol.SubmitInput{Text: "occupy", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}})
	if err != nil || resp.AsError() != nil {
		t.Fatal(err, resp.AsError())
	}
	eventID := strings.Repeat("d", 64)
	id, _ := RequestID(eventID)
	e.timing.Timeout = 40 * time.Millisecond
	e.timing.Poll = 5 * time.Millisecond
	first, err := e.run("hello", eventID, newTurn(), func(string, string) error { return nil })
	if err != nil || first.Meta.Remote.Code != string(protocol.CodeBusy) {
		t.Fatalf("first = %+v %v", first, err)
	}
	if !rt.Complete(busyID, "finished") {
		t.Fatal("no busy run")
	}
	got, err := e.run("hello", eventID, newTurn(), func(string, string) error { rt.Complete(id, "recovered"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != StopReasonEndTurn {
		t.Fatalf("busy redelivery = %+v", got)
	}
}

// TestRemoteBusyDMWaitsThenRuns covers: a DM that
// arrives while the session is busy is queued once, then runs on the same
// request id after the session is free.
// The waits below are event-driven, each bounded only by a deadlock guard:
// the earlier 1 s windows and 2 s turn budget failed on a loaded macOS CI
// runner (release PR #1092, ci run 39306655165) with reply_timeout.
func TestRemoteBusyDMWaitsThenRuns(t *testing.T) {
	rt := fake.New("fake", "e_1")
	e := remoteServer(t, rt, nil)
	e.timing.Poll = 5 * time.Millisecond
	e.timing.Timeout = time.Minute
	occupy := "d2c80e1d-feb7-4c10-959e-23456789abce"
	resp, err := ipc.Call(e.dir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: occupy, TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)), Input: &protocol.SubmitInput{Text: "local turn", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}})
	if err != nil || resp.AsError() != nil {
		t.Fatal(err, resp.AsError())
	}
	eventID := strings.Repeat("e", 64)
	id, err := RequestID(eventID)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var thoughts []string
	done := make(chan Result, 1)
	go func() {
		result, err := e.run("hello from buzz", eventID, newTurn(), func(update, text string) error {
			if update == "agent_thought_chunk" {
				mu.Lock()
				thoughts = append(thoughts, text)
				mu.Unlock()
			}
			return nil
		})
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- result
	}()
	guard := time.Now().Add(30 * time.Second)
	for time.Now().Before(guard) && queuedThoughts(&mu, &thoughts) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	queued := queuedThoughts(&mu, &thoughts)
	if queued != 1 {
		t.Fatalf("queued thoughts = %q", thoughts)
	}
	if rt.HasRun(id) {
		t.Fatal("dm ran while the session was busy")
	}
	if !rt.Complete(occupy, "local done") {
		t.Fatal("occupy did not complete")
	}
	for time.Now().Before(guard) {
		if rt.Complete(id, "ran after the wait") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := <-done
	if got.StopReason != StopReasonEndTurn {
		t.Fatalf("result = %+v", got)
	}
	if queuedThoughts(&mu, &thoughts) != 1 {
		mu.Lock()
		gotThoughts := append([]string(nil), thoughts...)
		mu.Unlock()
		t.Fatalf("queued thoughts after admit = %q", gotThoughts)
	}
}

func queuedThoughts(mu *sync.Mutex, thoughts *[]string) int {
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, text := range *thoughts {
		if strings.HasPrefix(text, "Queued:") {
			n++
		}
	}
	return n
}

// blockedSubmit is a fake whose Inspect advertises submit false with an
// adapter reason once blocked is set (core.SubmitBlocker).
type blockedSubmit struct {
	*fake.Runtime
	mu      sync.Mutex
	blocked string
}

func (b *blockedSubmit) block(reason string) { b.mu.Lock(); b.blocked = reason; b.mu.Unlock() }

func (b *blockedSubmit) Inspect() protocol.Session { s, _ := b.InspectSubmit(); return s }

func (b *blockedSubmit) InspectSubmit() (protocol.Session, string) {
	s := b.Runtime.Inspect()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blocked != "" {
		s.Capabilities.Submit = false
	}
	return s, b.blocked
}

// Regression: #923, a deferred
// unsupported refusal kept the adapter's reason on the record only, so the
// owner's DM showed a bare "rejected (unsupported)".
func TestDeferredRefusalReasonReachesTheDM(t *testing.T) {
	rt := fake.New("fake", "e_1")
	att := &blockedSubmit{Runtime: rt}
	dir := filepath.Join(endpointRoot(t), "extensions", "remote")
	store, err := requests.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	ep.Register(att)
	srv, err := ipc.Listen(dir, ep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served; _ = ep.Close(); _ = store.Close() })
	e := &remote{dir: dir, native: "fake", timing: Timing{Timeout: 2 * time.Second, Poll: 100 * time.Millisecond, Heartbeat: 10 * time.Millisecond}}
	e.timing.Poll = 5 * time.Millisecond

	rt.SetOffline(true)
	const reason = "the bridge is too old: install it and reload the session"
	var mu sync.Mutex
	var said []string
	done := make(chan Result, 1)
	go func() {
		result, err := e.run("hello", strings.Repeat("d", 64), newTurn(), func(update, text string) error {
			if update == "agent_message_chunk" {
				mu.Lock()
				said = append(said, text)
				mu.Unlock()
			}
			return nil
		})
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- result
	}()
	// Wait until the DM is deferred (received while offline), then let the
	// target return with submit blocked and admit the deferred record.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		recs, _, _ := store.ListWithPoison()
		if len(recs) == 1 && recs[0].State == protocol.StateReceived {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	att.block(reason)
	rt.SetOffline(false)
	if err := ep.Tick(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	mu.Lock()
	defer mu.Unlock()
	if got.Meta.Remote.State != string(protocol.StateRejected) || len(said) != 1 || !strings.Contains(said[0], reason) {
		t.Fatalf("result=%+v said=%q; want the rejection DM to carry %q", got.Meta.Remote, said, reason)
	}
}

// Regression: #959, an uncertain request
// posted its notice as the final answer, so the answer it later resolved to
// was suppressed on the next delivery. post keeps the machine agent's rule
// for one event's posts (postOnce): each kind at most once, and no status
// after the final.
func TestUncertainNoticeDoesNotHideTheAnswer(t *testing.T) {
	var posts []string
	posted := map[string]bool{}
	post := func(kind, text string) string {
		if posted[kind] || posted[PostFinal] {
			return "duplicate: this event already posted its " + kind + " text"
		}
		posted[kind] = true
		posts = append(posts, text)
		return "posted"
	}
	deliver := func(snap protocol.Snapshot) {
		turn := newTurn()
		r := &Turn{EventID: strings.Repeat("f", 64), Emit: func(string, string) error { return nil }, Done: turn.done, Settle: turn.settle, Post: post, Meta: Meta{Target: "fake"}}
		if _, err := r.settled("replied", snap); err != nil {
			t.Fatal(err)
		}
	}
	deliver(protocol.Snapshot{State: protocol.StateUncertain, RequestRef: "ref"})
	for range 2 {
		deliver(protocol.Snapshot{State: protocol.StateCompleted, RequestRef: "ref", Result: &protocol.Result{Text: "the answer"}})
	}
	if len(posts) != 2 || posts[1] != "the answer" {
		t.Fatalf("posts=%q; want one uncertainty notice and one answer", posts)
	}
}
