package claude

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	// Unix socket paths are limited to ~104 bytes on macOS; the t.TempDir
	// base is too long, so the test socket lives at a short /tmp path and
	// is removed with the test.
	sockPath := fmt.Sprintf("/tmp/cc-pr2test-%d-%d.sock", pid, time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(sockPath) })

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
	regDir := claudeSessionsDir(home)
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
	kf, err := peerKeyFile(home, pid, sockPath)
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
	kf, err := peerKeyFile(ft.home, 4242, ft.sockPath)
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

func TestConfirmationLadderFromTranscript(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	_ = att // attach only: the ladder test drives pollConfirmations directly
	admission, err := att.Submit(pr2BoundRequest("ladder-probe-body"))
	if err != nil {
		t.Fatal(err)
	}
	key := pr2Key()

	mkTranscript := func(home string, entries ...string) {
		dir := filepath.Join(home, ".claude", "projects", slugifyCwd("/tmp/proj"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sess-abc.jsonl"), []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	userLine := func(env string) string {
		return `{"type":"user","message":{"role":"user","content":"Another Claude session sent a message: ` + env + `"}}`
	}

	// Step 1: transcript carries the user line (harness accepted the
	// payload) -> SUBMITTED (still tentative class).
	var frameBody string
	ev, _ := att.Lookup(key, "")
	rec := att.runs[key]
	frameBody = rec.bodyMark
	if frameBody == "" {
		t.Fatal("run record lost")
	}
	mkTranscript(ft.home, userLine(frameBody))
	att.pollConfirmations()
	ev, _ = att.Lookup(key, "")
	if ev.Class != core.EvidenceTentative || !att.runs[key].submitted {
		t.Fatalf("after user line: class=%s submitted=%v, want tentative+submitted", ev.Class, att.runs[key].submitted)
	}

	// Step 2: an assistant entry after the user line -> the turn started
	// -> ADMITTED (confirmed).
	mkTranscript(ft.home, userLine(frameBody), `{"type":"assistant","message":{"role":"assistant","content":"working"}}`)
	att.pollConfirmations()
	ev, _ = att.Lookup(key, "")
	if ev.Class != core.EvidenceConfirmed || !ev.Admitted {
		t.Fatalf("after assistant line: %+v, want admitted (confirmed)", ev)
	}
	if admission.RunID != ev.RunID {
		t.Fatalf("RunID drift: admission %s vs evidence %s", admission.RunID, ev.RunID)
	}

	// Step 3: AcknowledgeResult releases the retained record.
	att.AcknowledgeResult(key, "", "")
	ev, _ = att.Lookup(key, "")
	if ev.Class != core.EvidenceUnknown {
		t.Fatalf("after ack: %s, want unknown (record released)", ev.Class)
	}
}

func TestTranscriptTailRefusesNonRegularLeaf(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", slugifyCwd("/tmp/proj"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Directory at the leaf (non-regular, portable to make): the tail
	// reader must refuse it, never block or read it.
	fifo := filepath.Join(dir, "sess-abc.jsonl")
	if err := os.Mkdir(fifo, 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- readTranscriptTail(home, "/tmp/proj", "sess-abc") }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("fifo read as %q, want empty", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readTranscriptTail blocked on a FIFO leaf — the lstat guard failed")
	}
}
