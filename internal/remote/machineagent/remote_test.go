package machineagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

// remoteEndpoint runs a real endpoint socket with att and returns the AMQ
// root it serves.
func remoteEndpoint(t *testing.T, att core.Attachment, crash core.CrashPoint) string {
	t.Helper()
	root := endpointRoot(t)
	dir := filepath.Join(root, remoteStateDir)
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
	return root
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
	if sock := ipc.SocketPath(filepath.Join(root, remoteStateDir)); runtime.GOOS != "windows" && len(sock) >= 104 {
		t.Fatalf("endpoint socket %s is %d bytes, over the 104-byte Unix socket path limit; set a shorter TMPDIR", sock, len(sock))
	}
	return root
}

func newTurn() *turnState {
	return &turnState{done: make(chan struct{})}
}

// A channel's owner message answers that the channel's session is not
// connected until that session is bound, then runs in the bound session.
// The turns run with no machine workers: they retire a record whose
// session has no binding.
func TestBindingModeFollowsTheBoundSession(t *testing.T) {
	f := standalone(t)
	rt := fake.New("fake", "e_1")
	b := directNative(remoteEndpoint(t, rt, nil), "fake")
	activeChannel(t, f.s, b, machineChanA)
	directTiming(f, time.Second)

	f.s.runOwnerTurn(machineChanA, f.ownerSays(t, machineChanA, "hello").Event, nil)
	const notConnected = "This channel's session is not connected. Run /amq-remote in that session to connect it again."
	if said := directContents(f.agentMessages(machineChanA)); len(said) != 1 || said[0] != notConnected {
		t.Fatalf("unbound: said=%q", said)
	}

	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	evt := f.ownerSays(t, machineChanA, "hello again").Event
	id, _ := remoteturn.RequestID(evt.ID.Hex())
	directComplete(t, rt, id, "from the bound session")
	f.s.runOwnerTurn(machineChanA, evt, nil)
	if !rt.HasRun(id) || f.posts(machineChanA, "from the bound session") != 1 {
		t.Fatalf("bound: ran=%v said=%q; want the bound session's answer", rt.HasRun(id), directContents(f.agentMessages(machineChanA)))
	}
}

// Regression: #885, a redelivered event followed the current binding, so a
// rebind between deliveries ran it again in the new session.
// A final answer is posted once per event, so the first delivery ends on
// its turn timeout with A's work still running, and the redelivery brings
// the answer.
func TestRedeliveryAfterRebindStaysOnTheFirstSession(t *testing.T) {
	f := standalone(t)
	a, b := fake.New("fake", "e_1"), fake.New("fake", "e_1")
	bindA, bindB := directNative(remoteEndpoint(t, a, nil), "fake"), directNative(remoteEndpoint(t, b, nil), "fake")
	if err := binding.WriteNamed(bindA); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, f.s, bindA, machineChanA)
	directTiming(f, 200*time.Millisecond)
	f.startMachine(t)
	in := f.ownerSays(t, machineChanA, "hello")
	id, _ := remoteturn.RequestID(in.Event.ID.Hex())
	waitClosed(t, f.s.Owner(in))
	if !a.Complete(id, "result from A") {
		t.Fatal("the first delivery did not run in session A")
	}
	if err := binding.WriteNamed(bindB); err != nil {
		t.Fatal(err)
	}
	directComplete(t, b, id, "result from B")
	waitClosed(t, f.s.Owner(in))
	if f.posts(machineChanA, "result from A") != 1 || f.posts(machineChanA, "result from B") != 0 || b.HasRun(id) {
		t.Fatalf("posts=%q ranInB=%v; the redelivery must return the first session's result", directContents(f.agentMessages(machineChanA)), b.HasRun(id))
	}
}

// keyRecorder is a fake runtime that reports the key of each submit.
type keyRecorder struct {
	*fake.Runtime
	keys chan requests.Key
}

func (k *keyRecorder) Submit(req core.BoundRequest) (core.Admission, error) {
	k.keys <- req.Key
	return k.Runtime.Submit(req)
}

// The session mirror tells a Codex turn the Buzz carrier sent by its user
// message's client_id. The carrier must record the clientUserMessageId the
// Codex attachment sends for the request the endpoint admitted.
func TestCodexCarrierRecordsTheClientUserMessageID(t *testing.T) {
	f := standalone(t)
	rt := &keyRecorder{Runtime: fake.New("fake", "e_1"), keys: make(chan requests.Key, 1)}
	b := directNative(remoteEndpoint(t, rt, nil), "alpha")
	b.Harness = "codex"
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, f.s, b, machineChanA)
	directTiming(f, time.Second)
	evt := f.ownerSays(t, machineChanA, "hello").Event
	id, _ := remoteturn.RequestID(evt.ID.Hex())
	directComplete(t, rt.Runtime, id, "done")
	f.s.runOwnerTurn(machineChanA, evt, nil)
	var key requests.Key
	select {
	case key = <-rt.keys:
	default:
		t.Fatal("the turn submitted nothing")
	}
	dir, err := mirror.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if sent := codex.ClientUserMessageID(key.CreatorHost, key.TargetID, key.RequestID); !mirror.CarriedIn(dir)(mirror.CodexCarriedID(sent)) {
		t.Fatalf("no carrier record for clientUserMessageId %q", sent)
	}
}

// canonicalTempDir is t.TempDir with symlinks resolved: the binding override
// refuses a symlinked path, and macOS temp dirs live under the /var symlink.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Regression: #885, a concurrent first delivery that lost the event claim
// used its own captured binding. The loser must follow the winner.
func TestEventClaimLoserFollowsTheWinner(t *testing.T) {
	f := standalone(t)
	eventID := strings.Repeat("b", 64)
	winner := binding.Binding{Root: "/winner", Target: "claude:1", NativeSession: "s-winner"}
	raw, _ := json.Marshal(eventClaim{Binding: winner, Name: "winner", Agent: f.s.id.Agent})
	if won, err := createExclusive(filepath.Join(f.s.eventDir(), eventID+".json"), raw); err != nil || !won {
		t.Fatalf("seed claim: won=%v err=%v", won, err)
	}
	loser := binding.Binding{Root: "/loser", Target: "claude:2", NativeSession: "s-loser", Name: "loser"}
	if err := binding.WriteNamed(loser); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, f.s, loser, machineChanA)
	got, err := f.s.turnBinding(routeSessionPrefix+machineChanA, eventID)
	if err != nil || !got.Same(winner) {
		t.Fatalf("turn binding = %+v %v; want the winner's", got, err)
	}
}

// Regression: #885, a reader used a visible claim before its directory entry
// was durable. Every returned claim is synced first.
func TestExistingEventClaimIsMadeDurableBeforeUse(t *testing.T) {
	f := standalone(t)
	eventID := strings.Repeat("c", 64)
	claimDir := f.s.eventDir()
	raw, _ := json.Marshal(eventClaim{Binding: binding.Binding{Root: "/r", Target: "claude:1", NativeSession: "s"}, Name: "r", Agent: f.s.id.Agent})
	if won, err := createExclusive(filepath.Join(claimDir, eventID+".json"), raw); err != nil || !won {
		t.Fatalf("seed claim: %v %v", won, err)
	}
	synced := false
	restore := fsq.SetPackageSyncDirFaultForTest(func(dir string) error {
		if dir == claimDir {
			synced = true
		}
		return nil
	})
	defer restore()
	if _, err := f.s.turnBinding(routeSessionPrefix+machineChanA, eventID); err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Fatal("an existing claim was returned before its directory was synced")
	}
}

// Regression: #959, one post marker per event
// let a timeout notice suppress the answer a later delivery brought, and a
// "Not connected" turn carried no event id, so it posted on every delivery.
func TestRedeliveryPostsEachKindOnce(t *testing.T) {
	t.Run("native timeout, then the answer", func(t *testing.T) {
		f := standalone(t)
		rt := fake.New("fake", "e_1")
		b := directNative(remoteEndpoint(t, rt, nil), "fake")
		if err := binding.WriteNamed(b); err != nil {
			t.Fatal(err)
		}
		activeChannel(t, f.s, b, machineChanA)
		directTiming(f, 100*time.Millisecond)
		f.startMachine(t)
		in := f.ownerSays(t, machineChanA, "hi")
		id, _ := remoteturn.RequestID(in.Event.ID.Hex())
		waitClosed(t, f.s.Owner(in))
		if !rt.Complete(id, "native answer") {
			t.Fatal("request was not running after the timeout")
		}
		for range 2 {
			waitClosed(t, f.s.Owner(in))
		}
		posts := directContents(f.agentMessages(machineChanA))
		if got := strings.Count(strings.Join(posts, "|"), "native answer"); got != 1 || len(posts) != 2 {
			t.Fatalf("posts=%q; want the timeout notice and the answer once each", posts)
		}
	})
	// The turns run with no machine workers: they retire a record whose
	// session has no binding.
	t.Run("not connected", func(t *testing.T) {
		f := standalone(t)
		activeChannel(t, f.s, directNative(canonicalTempDir(t), "fake"), machineChanA)
		evt := f.ownerSays(t, machineChanA, "hi").Event
		for range 2 {
			f.s.runOwnerTurn(machineChanA, evt, nil)
		}
		if posts := directContents(f.agentMessages(machineChanA)); len(posts) != 1 {
			t.Fatalf("posts=%q; want one not connected", posts)
		}
	})
}

// Regression: #959, a lost reply to a busy
// resubmit was recovered from the stored busy tombstone and posted as the
// final answer, so the answer a later delivery brought was suppressed.
func TestRecoveredBusyRefusalDoesNotHideTheAnswer(t *testing.T) {
	rt := fake.New("fake", "e_1")
	var loseReply sync.Mutex
	lose := false
	root := remoteEndpoint(t, rt, func(point string) error {
		loseReply.Lock()
		defer loseReply.Unlock()
		if point == core.PointBeforeDispatching && lose {
			lose = false
			return fmt.Errorf("reply lost")
		}
		return nil
	})
	f := standalone(t)
	b := directNative(root, "fake")
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, f.s, b, machineChanA)
	directTiming(f, 4*time.Second)
	f.startMachine(t)
	dir := filepath.Join(root, remoteStateDir)
	busyID := "d2c80e1d-feb7-4c10-959e-23456789abcf"
	resp, err := ipc.Call(dir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: busyID, TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)), Input: &protocol.SubmitInput{Text: "occupy", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}})
	if err != nil || resp.AsError() != nil {
		t.Fatal(err, resp.AsError())
	}
	in := f.ownerSays(t, machineChanA, "hello")
	id, _ := remoteturn.RequestID(in.Event.ID.Hex())
	ref := protocol.EncodeRef(ipc.LocalHost, "fake", id)
	stop, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		freed := false
		for {
			// Once the first submit is refused busy, the session frees up
			// within the turn's busy pause, and the reply to the next submit
			// is lost. Then the admitted run completes.
			if !freed && directBusy(dir, ref) {
				rt.Complete(busyID, "occupied")
				loseReply.Lock()
				lose = true
				loseReply.Unlock()
				freed = true
			}
			if freed && rt.Complete(id, "the answer") {
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	for range 3 {
		waitClosed(t, f.s.Owner(in))
	}
	if posts := directContents(f.agentMessages(machineChanA)); strings.Count(strings.Join(posts, "|"), "the answer") != 1 {
		t.Fatalf("posts=%q; want the answer once", posts)
	}
}
