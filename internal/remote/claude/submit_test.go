package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// fakeTarget wires a full hermetic target: a temp HOME with a session
// registry pointing at a listening unix socket, the matching .key file
// minted the way the harness mints it, and a capture channel for the
// bytes the adapter writes.
type fakeTarget struct {
	home     string
	sockPath string
	ln       net.Listener
	received chan []byte
}

// stopListener stops accepting; the socket file stays (a dead process).

func newFakeTarget(t *testing.T, pid int, features []string) *fakeTarget {
	t.Helper()
	home := t.TempDir()
	// Unix socket paths are limited to ~104 bytes on macOS and the t.TempDir
	// base is too long, so the socket lives in a short directory directly
	// under the temp root, which honors TMPDIR (agent-message-queue-8p9).
	sockDir, err := os.MkdirTemp("", "cc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, fmt.Sprintf("%d.sock", pid))

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the socket file after Close so the dead-target test dials a
	// stale socket (connection refused), not a missing path.
	ln.SetUnlinkOnClose(false)
	ft := &fakeTarget{home: home, sockPath: sockPath, ln: ln, received: make(chan []byte, 4)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 16<<10)
				n, _ := conn.Read(buf)
				if n > 0 {
					select {
					case ft.received <- buf[:n]:
					default:
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	// Session registry: the target publishes its messaging socket path.
	regDir := registryDir(defaultConfigDir(home))
	if err := os.MkdirAll(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg := map[string]any{
		"pid": pid, "sessionId": "sess-abc", "kind": "interactive",
		"cwd": "/tmp/proj", "messagingSocketPath": sockPath,
		"peerFeatures": features,
	}
	raw, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(regDir, fmt.Sprintf("%d.json", pid)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The harness-minted key file: <pid>.<sha256(resolved sock)>.key.
	kf, err := peerKeyFile(defaultConfigDir(home), pid, sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(kf), 0o700); err != nil {
		t.Fatal(err)
	}
	key, _ := json.Marshal(map[string]string{"peerToken": "00000000000000000000000000000000"})
	if err := os.WriteFile(kf, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return ft
}

func (ft *fakeTarget) stopListener() error { return ft.ln.Close() }

func (ft *fakeTarget) attach(t *testing.T) *Attachment {
	t.Helper()
	att, err := Attach(config{Pid: 4242, Home: ft.home, Target: "cc-1"})
	if err != nil {
		t.Fatal(err)
	}
	return att
}

func pr2Key() requests.Key {
	return requests.Key{CreatorHost: "local", TargetID: "cc-1", RequestID: "22222222-2222-4222-8222-222222222222"}
}

func pr2BoundRequest(text string) core.BoundRequest {
	return core.BoundRequest{
		Key:      pr2Key(),
		Epoch:    "e1",
		Input:    protocol.SubmitInput{Text: text, Deliver: protocol.DeliverTurn, Busy: protocol.BusyReject},
		NotAfter: "2099-01-01T00:00:00Z",
	}
}

// waitRecv drains the fake socket with a deadline.
func waitRecv(t *testing.T, ft *fakeTarget) []byte {
	t.Helper()
	select {
	case b := <-ft.received:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("fake socket received nothing within 3s")
		return nil
	}
}

func TestSubmitDeliversPinnedWireOverSocket(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	admission, err := att.Submit(pr2BoundRequest("hello from amq"))
	if err != nil {
		t.Fatalf("Submit errored: %v", err)
	}
	if !admission.Admitted {
		t.Fatalf("Submit refused: %+v (%s)", admission, admission.Message)
	}
	raw := waitRecv(t, ft)

	// Line 1: the mandatory auth line with the peer token.
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("wire = %d lines, want auth+frame", len(lines))
	}
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &auth); err != nil {
		t.Fatalf("auth line: %v", err)
	}
	if auth.Type != "auth" || auth.Token != "00000000000000000000000000000000" {
		t.Fatalf("auth line wrong: %+v", auth)
	}
	// Line 2: the user frame with the envelope, from-mode set.
	var frame Frame
	if err := json.Unmarshal([]byte(lines[1]), &frame); err != nil {
		t.Fatalf("frame line: %v", err)
	}
	if frame.Type != "user" || frame.Message.Role != "user" {
		t.Fatalf("frame not a user message: %+v", frame)
	}
	if !strings.Contains(frame.Message.Content, `<cross-session-message`) ||
		!strings.Contains(frame.Message.Content, `from-mode="code"`) ||
		!strings.Contains(frame.Message.Content, "hello from amq") {
		t.Fatalf("envelope missing pieces: %q", frame.Message.Content)
	}

	// The send is only ever TENTATIVE — no socket-based upgrade.
	ev, err := att.Lookup(pr2Key(), "")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Class != core.EvidenceTentative {
		t.Fatalf("evidence after socket send = %s, want tentative", ev.Class)
	}
}

func TestSubmitRefusesWhenKeyFileMissing(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	// Remove the minted key: the inbound precondition is not met even
	// though the socket listens.
	kf, err := peerKeyFile(defaultConfigDir(ft.home), 4242, ft.sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(kf); err != nil {
		t.Fatal(err)
	}
	att := ft.attach(t)
	admission, err := att.Submit(pr2BoundRequest("x"))
	if err != nil {
		t.Fatal(err)
	}
	if admission.Admitted || !strings.Contains(admission.Message, "messaging socket") {
		t.Fatalf("want inbound-precondition refusal, got %+v (%s)", admission, admission.Message)
	}
	select {
	case b := <-ft.received:
		t.Fatalf("refused submit still wrote %d bytes to the socket", len(b))
	default:
	}
}

func TestSubmitDialFailureIsDefinitiveNotSent(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	// A dead target: stop the listener but leave the socket file in place
	// (a crashed/stuck Claude Code process), so the dial is refused.
	_ = ft.stopListener()
	att := ft.attach(t)
	admission, err := att.Submit(pr2BoundRequest("x"))
	if err != nil {
		t.Fatal(err)
	}
	if admission.Admitted {
		t.Fatal("dial failure must never admit")
	}
	if !strings.Contains(admission.Message, ErrNotSent.Error()) {
		t.Fatalf("want the not-sent refusal shape, got %q", admission.Message)
	}
}

func TestLookupUnknownForNeverSubmittedKey(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	ev, err := att.Lookup(requests.Key{CreatorHost: "local", TargetID: "cc-1", RequestID: "33333333-3333-4333-8333-333333333333"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// The socket wire has no admission primitive: Unknown, never None
	// (a lost submit and a never-submitted key are indistinguishable).
	if ev.Class != core.EvidenceUnknown || !ev.Known {
		t.Fatalf("lookup = %+v, want Known Unknown", ev)
	}
}

// A Buzz request recorded for session A, whose pid runs session B by the
// time the endpoint recovers it after a crash after the received write
// (/clear or /resume), is refused before dispatch and B gets zero bytes
// (agent-message-queue-611.70: session B received the prompt).
func TestSessionChangedBeforeDispatchSendsNothingAfterRestart(t *testing.T) {
	pid := os.Getpid() // a live pid, so the target reads attached
	ftA := newFakeTarget(t, pid, nil)
	ftB := newFakeTarget(t, pid, nil) // only its socket is used
	stateDir := t.TempDir()

	store1, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep1 := core.New(core.Config{Store: store1, Crash: func(p string) error {
		if p == core.PointAfterReceived {
			return errors.New("power loss")
		}
		return nil
	}})
	att1, err := Attach(config{Pid: pid, Home: ftA.home, Target: "cc-1"})
	if err != nil {
		t.Fatal(err)
	}
	ep1.Register(att1)
	// What the Buzz carrier sends after its fence proved sess-abc attached.
	src := core.Source{Host: "buzz-0123456789abcdef", NativeSession: "sess-abc",
		Origin: map[string]string{"carrier": "buzz", "body": "b", "channel": "dm-1", "event": "e1"}}
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "33333333-3333-4333-8333-333333333333", TargetID: "cc-1", Epoch: SentinelUnpinned,
		NotAfter: protocol.FormatTime(time.Now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "authorized for A", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn, MinEvidence: protocol.EvidenceSubmitted}}
	if _, err := ep1.Handle(cmd, src); !errors.Is(err, core.ErrCrashed) {
		t.Fatalf("submit = %v, want the crash after received", err)
	}
	_ = ep1.Close()
	_ = store1.Close()

	// The same pid now runs session B with its own socket.
	reg, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "sess-B", "kind": "interactive", "cwd": "/tmp/proj", "messagingSocketPath": ftB.sockPath})
	if err := os.WriteFile(filepath.Join(registryDir(defaultConfigDir(ftA.home)), fmt.Sprintf("%d.json", pid)), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	kf, err := peerKeyFile(defaultConfigDir(ftA.home), pid, ftB.sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kf, []byte(`{"peerToken":"00000000000000000000000000000000"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	store2, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep2 := core.New(core.Config{Store: store2})
	t.Cleanup(func() { _ = ep2.Close(); _ = store2.Close() })
	att2, err := Attach(config{Pid: pid, Home: ftA.home, Target: "cc-1"})
	if err != nil {
		t.Fatal(err)
	}
	ep2.Register(att2)
	if err := ep2.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	key := requests.Key{CreatorHost: src.Host, TargetID: "cc-1", RequestID: cmd.RequestID}
	rec, _, err := store2.Get(key)
	if err != nil || rec == nil || rec.State != protocol.StateRejected || rec.Code != protocol.CodeSessionChanged || rec.NativeDispatches != 0 {
		t.Fatalf("recovered record = %+v, %v; want rejected session_changed before any dispatch", rec, err)
	}
	// The adapter refuses too, at the write: the fence-to-Submit race.
	adm, err := att2.Submit(core.BoundRequest{Key: key, Epoch: SentinelUnpinned, Input: *cmd.Input, NotAfter: cmd.NotAfter, NativeSession: "sess-abc"})
	if err != nil || adm.Code != protocol.CodeSessionChanged {
		t.Fatalf("adapter Submit = %+v, %v; want session_changed", adm, err)
	}
	select {
	case b := <-ftB.received:
		t.Fatalf("session B received %d bytes of the prompt for session A", len(b))
	case <-time.After(200 * time.Millisecond):
	}
}

// A request whose not_after passed is never written to Claude
// (agent-message-queue-611.70: the adapter wrote it anyway).
func TestSubmitWritesNothingAfterNotAfter(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	req := pr2BoundRequest("expired prompt")
	req.NotAfter = "2000-01-01T00:00:00Z"
	adm, err := att.Submit(req)
	if err != nil || adm.Admitted || adm.Code != protocol.CodeExpired {
		t.Fatalf("Submit = %+v, %v; want expired", adm, err)
	}
	select {
	case b := <-ft.received:
		t.Fatalf("adapter wrote %d bytes after not_after", len(b))
	case <-time.After(200 * time.Millisecond):
	}
}

// A Lookup while the frame is still being written reads the run as
// tentative, never as a lost run that core would record as uncertain
// (audit 2026-11-10).
func TestLookupDuringSocketWriteIsTentative(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	// Replace the listener with one that looks the key up before it reads a
	// byte. The frame is larger than any socket buffer, so the write cannot
	// finish until that lookup is done.
	_ = ft.stopListener()
	if err := os.Remove(ft.sockPath); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", ft.sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	seen := make(chan core.Evidence, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		ev, _ := att.Lookup(pr2Key(), "")
		seen <- ev
		_, _ = io.Copy(io.Discard, conn)
	}()
	adm, err := att.Submit(pr2BoundRequest(strings.Repeat("x", 4<<20)))
	if err != nil || !adm.Admitted {
		t.Fatalf("Submit = %+v, %v; want admitted", adm, err)
	}
	if ev := <-seen; ev.Class != core.EvidenceTentative {
		t.Fatalf("lookup during the write = %s, want tentative", ev.Class)
	}
}

// A frame written but never shown in the session's transcript leaves the
// run's admission unknown once its window closes, instead of running for
// ever (audit 2026-11-10).
func TestLookupOfAnUndeliveredFrameIsUnknownAfterItsWindow(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	start := time.Now()
	var skew atomic.Int64 // the poller reads the clock too
	att.clock = func() time.Time { return start.Add(time.Duration(skew.Load())) }
	req := pr2BoundRequest("hello")
	req.NotAfter = protocol.FormatTime(start.Add(time.Minute))
	if adm, err := att.Submit(req); err != nil || !adm.Admitted {
		t.Fatalf("Submit = %+v, %v; want admitted", adm, err)
	}
	waitRecv(t, ft)
	if ev, _ := att.Lookup(pr2Key(), ""); ev.Class != core.EvidenceTentative {
		t.Fatalf("lookup inside the window = %s, want tentative", ev.Class)
	}
	skew.Store(int64(2 * time.Minute))
	if ev, _ := att.Lookup(pr2Key(), ""); ev.Class != core.EvidenceUnknown {
		t.Fatalf("lookup after the window = %s, want unknown", ev.Class)
	}
}

// releaseFixture is an endpoint over one attached Claude target whose
// adapter clock the test moves.
func releaseFixture(t *testing.T) (*core.Endpoint, *requests.Store, *Attachment, *fakeTarget, *atomic.Int64) {
	t.Helper()
	pid := os.Getpid() // a live pid, so the target reads attached
	ft := newFakeTarget(t, pid, nil)
	store, err := requests.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	t.Cleanup(func() { _ = ep.Close(); _ = store.Close() })
	att, err := Attach(config{Pid: pid, Home: ft.home, Target: "cc-1"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	skew := new(atomic.Int64) // the poller reads the clock too
	att.clock = func() time.Time { return start.Add(time.Duration(skew.Load())) }
	ep.Register(att)
	return ep, store, att, ft, skew
}

func releaseSubmit(t *testing.T, ep *core.Endpoint, id string, window time.Duration) protocol.Reply {
	t.Helper()
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "cc-1", Epoch: SentinelUnpinned,
		NotAfter: protocol.FormatTime(time.Now().Add(window)),
		Input:    &protocol.SubmitInput{Text: "prompt " + id, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}
	out, err := ep.Handle(cmd, core.Source{Host: core.LocalHost})
	if err != nil {
		t.Fatalf("submit %s: %v", id, err)
	}
	return out.(protocol.Reply)
}

// An uncertain record held its Claude target busy for good. The owner's
// release ends it failed released_by_owner, the target admits a new
// request, and a late delivery of the released frame completes neither
// request (audit 2026-11-10).
func TestReleaseUncertainFreesTheTargetAndDisownsTheLateFrame(t *testing.T) {
	ep, store, att, ft, skew := releaseFixture(t)
	old := releaseSubmit(t, ep, "44444444-4444-4444-8444-444444444444", time.Minute)
	oldEnv := frameEnvelope(t, ft)
	skew.Store(int64(2 * time.Minute)) // the window closes with no delivery
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	oldKey := requests.Key{CreatorHost: core.LocalHost, TargetID: "cc-1", RequestID: old.Snapshot.RequestID}
	if rec, _, _ := store.Get(oldKey); rec.State != protocol.StateUncertain {
		t.Fatalf("old record = %s, want uncertain", rec.State)
	}
	if busy := releaseSubmit(t, ep, "55555555-5555-4555-8555-555555555555", 5*time.Minute); busy.Snapshot.Code != protocol.CodeBusy {
		t.Fatalf("submit while uncertain = %s/%s, want busy", busy.Snapshot.State, busy.Snapshot.Code)
	}

	rep, err := ep.Release(old.Snapshot.RequestRef)
	if err != nil || rep.Snapshot.State != protocol.StateFailed || rep.Snapshot.Code != protocol.CodeReleasedByOwner {
		t.Fatalf("release = %+v, %v; want failed released_by_owner", rep.Snapshot, err)
	}
	next := releaseSubmit(t, ep, "66666666-6666-4666-8666-666666666666", 5*time.Minute)
	if next.Snapshot.State != protocol.StateRunning {
		t.Fatalf("submit after release = %s/%s, want running", next.Snapshot.State, next.Snapshot.Code)
	}
	waitRecv(t, ft)

	// The released frame lands now and its turn ends with a Stop that the
	// new, not yet delivered request could claim by time.
	appendTranscript(t, ft.home, deliveredLine(t, frameMsgID(oldKey), oldEnv), transcriptLine(t, "assistant", "late answer"))
	appendStopMarker(t, ft.home, att.now().UnixMilli()+1)
	att.pollConfirmations()
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := store.Get(oldKey); rec.State != protocol.StateFailed || rec.Code != protocol.CodeReleasedByOwner || rec.Result != nil {
		t.Fatalf("old record after the late frame = %s/%s result=%+v, want failed released_by_owner with no result", rec.State, rec.Code, rec.Result)
	}
	nextKey := requests.Key{CreatorHost: core.LocalHost, TargetID: "cc-1", RequestID: next.Snapshot.RequestID}
	if rec, _, _ := store.Get(nextKey); rec.State != protocol.StateRunning || rec.Result != nil {
		t.Fatalf("new record after the late frame = %s result=%+v, want running with no result", rec.State, rec.Result)
	}
}

// Release acts only on an uncertain record: a running one is refused and
// stays as it was (audit 2026-11-10).
func TestReleaseRefusesARunningRecord(t *testing.T) {
	ep, store, _, ft, _ := releaseFixture(t)
	rep := releaseSubmit(t, ep, "77777777-7777-4777-8777-777777777777", time.Minute)
	waitRecv(t, ft)
	_, err := ep.Release(rep.Snapshot.RequestRef)
	var r *protocol.Refusal
	if !errors.As(err, &r) || protocol.ExitForCode(r.Code) != protocol.ExitActionRequired {
		t.Fatalf("release of a running record = %v, want an action-required refusal", err)
	}
	key := requests.Key{CreatorHost: core.LocalHost, TargetID: "cc-1", RequestID: rep.Snapshot.RequestID}
	if rec, _, _ := store.Get(key); rec.State != protocol.StateRunning || rec.Revision != rep.Snapshot.Revision {
		t.Fatalf("record after the refused release = %s rev %d, want running rev %d", rec.State, rec.Revision, rep.Snapshot.Revision)
	}
}
