//go:build !windows

package buzzio

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Bead 611.42.3, design section 7: a Claude tool approval raised during a
// Buzz request shows in the DM. Through the real carrier, endpoint and
// Claude attachment on a temporary home, with the PermissionRequest hook
// called as a function: ❌ prints deny, and the approval message is edited
// with the answer that was sent.
func TestClaudeApprovalRejectedFromBuzz(t *testing.T) {
	e := newClaudeApprovalE2E(t)
	react := nostr.Event{CreatedAt: nostr.Timestamp(e.advance().Unix()), Kind: KindReaction, Content: "❌", Tags: nostr.Tags{{"e", e.msg.ID.Hex()}}}
	if err := react.Sign(e.owner); err != nil {
		t.Fatal(err)
	}
	if err := e.c.IngestReaction(react); err != nil {
		t.Fatal(err)
	}
	var ans map[string]string
	if data, err := os.ReadFile(filepath.Join(e.dir, "answers", e.iid+".json")); err != nil || json.Unmarshal(data, &ans) != nil || ans["option"] != "deny" {
		t.Fatalf("answer file = %v (%v), want deny", ans, err)
	}
	e.hookExited()
	want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Rejected from Buzz by the owner."}}}`
	if got := strings.TrimSpace(e.out.String()); got != want {
		t.Fatalf("hook printed %s, want %s", got, want)
	}
	if res := e.resolved(); res["outcome"] != "hook_claimed" || res["option"] != "deny" {
		t.Fatalf("resolved = %v, want the hook's claim for deny", res)
	}
	var del map[string]any
	if data, err := os.ReadFile(filepath.Join(e.dir, "delivery", e.iid+".json")); err != nil || json.Unmarshal(data, &del) != nil || del["written"] != true {
		t.Fatalf("delivery = %v (%v), want the deny written", del, err)
	}
	e.await(approvalKey(e.ref, e.iid) + "/outcome")
	e.flush()
	if !e.edited("Reject was sent from Buzz") {
		t.Fatalf("sent = %+v, want the approval message edited with the sent reject", e.sent)
	}
}

// Pro review of #929, 2026-09-30, #1: an answer file is not owner
// authorization. Without an owner pin the DM offers ❌ only, and a forged
// allow answer file never yields an allow: the hook prints nothing, and the
// approval does not end as answered.
func TestClaudeForgedAllowAnswerNeverAllows(t *testing.T) {
	e := newClaudeApprovalE2E(t)
	if strings.Contains(e.msg.Content, "✅") || !strings.Contains(e.msg.Content, "React ❌ to reject") {
		t.Fatalf("approval message = %q, want reject only", e.msg.Content)
	}
	var req map[string]any
	if data, err := os.ReadFile(filepath.Join(e.dir, "requests", e.iid+".json")); err != nil || json.Unmarshal(data, &req) != nil {
		t.Fatalf("request = %v (%v)", req, err)
	}
	forged, _ := json.Marshal(map[string]any{"interaction_id": e.iid, "action_hash": req["action_hash"], "option": "allow", "at": "x"})
	if err := os.MkdirAll(filepath.Join(e.dir, "answers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "answers", e.iid+".json"), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(e.errs.String(), "ignored"); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hook never read the forged answer")
		}
	}
	close(e.done) // the terminal decides
	e.hookExited()
	if e.out.Len() != 0 {
		t.Fatalf("hook printed %q, want no decision", e.out.String())
	}
	if res := e.resolved(); res["outcome"] == "answered" || res["outcome"] == "hook_claimed" {
		t.Fatalf("resolved = %v, want an outcome other than answered", res)
	}
	e.await(approvalKey(e.ref, e.iid) + "/outcome")
	e.flush()
	if e.edited("was sent from Buzz") || !e.edited("Closed outside Buzz") {
		t.Fatalf("sent = %+v, want the message edited as closed outside Buzz", e.sent)
	}
}

// claudeApprovalE2E is a Buzz request to a fake Claude session whose tool
// call raised a PermissionRequest, with the approval message posted.
type claudeApprovalE2E struct {
	t       *testing.T
	c       *Carrier
	ep      *core.Endpoint
	ledger  *Ledger
	owner   [32]byte
	body    [32]byte
	advance func() time.Time
	sent    []nostr.Event
	// published is every event flushed so far: the relay the edit
	// fetcher reads.
	published []nostr.Event
	msg       nostr.Event
	ref, iid  string
	dir       string
	out       bytes.Buffer
	errs      lockedBuffer
	done      chan struct{}
	exited    chan struct{}
}

func newClaudeApprovalE2E(t *testing.T) *claudeApprovalE2E {
	return newClaudeApprovalE2EWith(t, false, "go test ./...")
}

// newClaudeApprovalE2EWith raises the approval for command. pinned installs
// the hook with the owner's pubkey on its command line, pins the share with
// that owner, and runs the hook with that owner and the real verifier.
func newClaudeApprovalE2EWith(t *testing.T, pinned bool, command string) *claudeApprovalE2E {
	base := time.Now().Truncate(time.Second)
	clock := func() time.Time { return base }
	var tick atomic.Int64
	e := &claudeApprovalE2E{t: t, done: make(chan struct{}), exited: make(chan struct{})}
	e.advance = func() time.Time { return base.Add(time.Duration(tick.Add(1)) * time.Second) }

	home := t.TempDir()
	const sid, cwd = "sess-1", "/work/proj"
	frames := fakeClaudeSession(t, home, sid, cwd)
	_, _ = rand.Read(e.owner[:])
	_, _ = rand.Read(e.body[:])
	owner, verify := "", claude.AllowVerifier(nil)
	if pinned {
		owner, verify = nostr.GetPublicKey(e.owner).Hex(), VerifyApproveEvidence
		if err := claude.InstallPermissionHook(home, "/opt/amq-remote", claude.DefaultPermissionWait, owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := claude.PinApprovals(home, sid, "share-1", owner, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "home": home, "approve": true})
	att, err := claude.Factory(context.Background(), registry.FactoryConfig{Target: "cc-1", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	att.(*claude.Attachment).SetNow(clock)
	store, err := requests.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	e.ep = core.New(core.Config{Store: store, Now: clock, Publish: func(s protocol.Snapshot, origin map[string]string) error { return e.c.Publish(s, origin) }})
	t.Cleanup(func() { _ = e.ep.Close() })
	e.ep.Register(att)

	body := e.body
	b := Binding{Owner: nostr.GetPublicKey(e.owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cc-1", RelayHost: "relay", NativeSession: sid,
		MinEvidence: protocol.EvidenceSubmitted}
	e.ledger, _ = OpenLedger(t.TempDir())
	e.c = NewCarrier(e.ledger, b, body, ownerGrant(t, e.owner, b.Body, KindDM, KindEdit), e.ep.NativeSessionID, e.ep.Handle)
	e.c.now = e.advance
	e.c.SetEditFetcher(func(_ context.Context, id string) ([]nostr.Event, error) {
		var edits []nostr.Event
		for _, evt := range e.published {
			if evt.Kind == KindEdit && evt.PubKey.Hex() == b.Body && lastETag(evt) == id {
				edits = append(edits, evt)
			}
		}
		return edits, nil
	})

	dm := ownerEvent(t, e.owner, "dm-1", "run the tests", clock())
	n, err := Normalize(dm, b, clock())
	if err != nil {
		t.Fatal(err)
	}
	e.ref = protocol.EncodeRef(e.c.source(dm.ID.Hex(), "").Host, "cc-1", n.RequestID)
	if err := e.c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	var msgID string
	select {
	case msgID = <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("the Claude session received no frame")
	}
	// Claude writes the peer delivery as its own turn, then the tool call.
	transcript := filepath.Join(home, ".claude", "projects", "-work-proj", sid+".jsonl")
	writeLines(t, transcript,
		map[string]any{"type": "user", "promptId": "p-1", "timestamp": base.UTC().Format(time.RFC3339Nano),
			"message": map[string]any{"role": "user", "content": "<cross-session-message>run the tests</cross-session-message>"},
			"origin":  map[string]any{"kind": "peer", "msg_id": msgID}},
		map[string]any{"type": "assistant", "timestamp": base.UTC().Format(time.RFC3339Nano),
			"message": map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": command}}}}})

	stdin, _ := json.Marshal(map[string]any{"session_id": sid, "prompt_id": "p-1", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": command}})
	go func() {
		defer close(e.exited)
		claude.RunPermissionHook(home, bytes.NewReader(stdin), &e.out, &e.errs, e.done, time.Minute, owner, verify)
	}()
	e.dir = filepath.Join(home, ".claude", "sessions", "amq-approve", sid)
	for deadline := time.Now().Add(5 * time.Second); e.iid == ""; time.Sleep(time.Millisecond) {
		entries, _ := os.ReadDir(filepath.Join(e.dir, "requests"))
		for _, ent := range entries {
			if id, ok := strings.CutSuffix(ent.Name(), ".json"); ok && strings.HasPrefix(id, "cc-") {
				e.iid = id
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook raised no request")
		}
	}
	e.await(approvalKey(e.ref, e.iid))
	e.flush()
	for _, evt := range e.sent {
		if strings.Contains(evt.Content, "Approval needed") {
			e.msg = evt
		}
	}
	if !strings.Contains(e.msg.Content, "Approval needed") || !strings.Contains(e.msg.Content, "❌ to reject") {
		t.Fatalf("approval message = %q, want the command and how to reject", e.msg.Content)
	}
	return e
}

func (e *claudeApprovalE2E) flush() {
	e.t.Helper()
	e.sent = nil
	if err := e.c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error {
		e.sent, e.published = append(e.sent, evt), append(e.published, evt)
		return nil
	}, nil); err != nil {
		e.t.Fatal(err)
	}
}

// await waits, bounded, until the outbox holds key, reconciling meanwhile.
func (e *claudeApprovalE2E) await(key string) {
	e.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, ok, err := e.ledger.Prepared(key); err != nil || ok {
			if err != nil {
				e.t.Fatal(err)
			}
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s was never prepared", key)
		}
		if err := e.ep.Reconcile(); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *claudeApprovalE2E) hookExited() {
	e.t.Helper()
	select {
	case <-e.exited:
	case <-time.After(5 * time.Second):
		e.t.Fatal("the hook did not exit")
	}
}

func (e *claudeApprovalE2E) resolved() map[string]string {
	e.t.Helper()
	var res map[string]string
	data, err := os.ReadFile(filepath.Join(e.dir, "resolved", e.iid+".json"))
	if err != nil || json.Unmarshal(data, &res) != nil {
		e.t.Fatalf("resolved = %s (%v)", data, err)
	}
	return res
}

// edited reports an edit of the approval message that contains text.
func (e *claudeApprovalE2E) edited(text string) bool {
	for _, evt := range e.sent {
		if evt.Kind == KindEdit && tagValue(evt, "e") == e.msg.ID.Hex() && strings.Contains(evt.Content, text) {
			return true
		}
	}
	return false
}

// lockedBuffer is a bytes.Buffer safe for the hook goroutine and the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeClaudeSession lays down what a live interactive Claude session shows
// on disk for this process: the session registry, its cross-session socket
// and the peer key file. It returns the msg_id of each frame received.
func fakeClaudeSession(t *testing.T, home, sid, cwd string) <-chan string {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "cc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	frames := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			sc := bufio.NewScanner(conn)
			for sc.Scan() {
				var f struct {
					MsgID string `json:"msg_id"`
				}
				if json.Unmarshal(sc.Bytes(), &f) == nil && f.MsgID != "" {
					frames <- f.MsgID
				}
			}
			_ = conn.Close()
		}
	}()
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	reg, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": sid, "kind": "interactive", "cwd": cwd, "messagingSocketPath": sock, "status": "idle"})
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(sock))
	key, _ := json.Marshal(map[string]string{"peerToken": strings.Repeat("0", 32)})
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:]))), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects", "-work-proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	return frames
}

func writeLines(t *testing.T, path string, lines ...map[string]any) {
	t.Helper()
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		appendLine(t, path, string(raw))
	}
}
