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
// Buzz request is answered from the DM. Through the real carrier, endpoint
// and Claude attachment on a temporary home, with the PermissionRequest
// hook called as a function: ✅ prints allow, ❌ prints deny, and the DM
// approval message is edited with the answer that was sent.
func TestClaudeApprovalApprovedFromBuzz(t *testing.T) {
	claudeApprovalFromBuzz(t, "✅", `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`, "allow", "Approve was sent from Buzz")
}

func TestClaudeApprovalRejectedFromBuzz(t *testing.T) {
	claudeApprovalFromBuzz(t, "❌", `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"Rejected from Buzz by the owner."}}}`, "deny", "Reject was sent from Buzz")
}

func claudeApprovalFromBuzz(t *testing.T, gesture, decision, option, outcome string) {
	base := time.Now().Truncate(time.Second)
	clock := func() time.Time { return base }
	var tick atomic.Int64
	advance := func() time.Time { return base.Add(time.Duration(tick.Add(1)) * time.Second) }

	home := t.TempDir()
	const sid, cwd = "sess-1", "/work/proj"
	frames := fakeClaudeSession(t, home, sid, cwd)
	if _, err := claude.PinApprovals(home, sid, "share-1", os.Getpid()); err != nil {
		t.Fatal(err)
	}

	cfg, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "home": home, "approve": true})
	att, err := claude.Factory(context.Background(), registry.FactoryConfig{Target: "cc-1", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	att.(*claude.Attachment).SetNow(clock)
	root := t.TempDir()
	store, err := requests.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	var c *Carrier
	ep := core.New(core.Config{Store: store, Now: clock, Publish: func(s protocol.Snapshot, origin map[string]string) error { return c.Publish(s, origin) }})
	defer func() { _ = ep.Close() }()
	ep.Register(att)

	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cc-1", RelayHost: "relay", NativeSession: sid,
		MinEvidence: protocol.EvidenceSubmitted}
	ledger, _ := OpenLedger(t.TempDir())
	c = NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), ep.NativeSessionID, ep.Handle)
	c.now = advance
	var sent []nostr.Event
	flush := func() {
		sent = nil
		if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil); err != nil {
			t.Fatal(err)
		}
	}
	await := func(key string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			if _, ok, err := ledger.Prepared(key); err != nil || ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never prepared", key)
			}
			if err := ep.Reconcile(); err != nil {
				t.Fatal(err)
			}
		}
	}

	dm := ownerEvent(t, owner, "dm-1", "run the tests", clock())
	n, err := Normalize(dm, b, clock())
	if err != nil {
		t.Fatal(err)
	}
	ref := protocol.EncodeRef(c.source(dm.ID.Hex(), "").Host, "cc-1", n.RequestID)
	if err := c.Ingest(dm); err != nil {
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
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "go test ./..."}}}}})

	stdin, _ := json.Marshal(map[string]any{"session_id": sid, "prompt_id": "p-1", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./..."}})
	var out bytes.Buffer
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		claude.RunPermissionHook(home, bytes.NewReader(stdin), &out, make(chan struct{}), time.Minute)
	}()
	approveDir := filepath.Join(home, ".claude", "sessions", "amq-approve", sid)
	iid := ""
	for deadline := time.Now().Add(5 * time.Second); iid == ""; time.Sleep(time.Millisecond) {
		entries, _ := os.ReadDir(filepath.Join(approveDir, "requests"))
		for _, e := range entries {
			if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && strings.HasPrefix(id, "cc-") {
				iid = id
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook raised no request")
		}
	}

	await(approvalKey(ref, iid))
	flush()
	var msg nostr.Event
	for _, evt := range sent {
		if strings.Contains(evt.Content, "Approval needed") {
			msg = evt
		}
	}
	if !strings.Contains(msg.Content, "go test ./...") || !strings.Contains(msg.Content, "React ✅") {
		t.Fatalf("approval message = %q, want the command and how to answer", msg.Content)
	}

	react := nostr.Event{CreatedAt: nostr.Timestamp(advance().Unix()), Kind: KindReaction, Content: gesture, Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestReaction(react); err != nil {
		t.Fatal(err)
	}
	var ans map[string]string
	if data, err := os.ReadFile(filepath.Join(approveDir, "answers", iid+".json")); err != nil || json.Unmarshal(data, &ans) != nil || ans["option"] != option {
		t.Fatalf("answer file = %v (%v), want %s", ans, err, option)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not exit after the answer")
	}
	if got := strings.TrimSpace(out.String()); got != decision {
		t.Fatalf("hook printed %s, want %s", got, decision)
	}
	var res map[string]string
	if data, err := os.ReadFile(filepath.Join(approveDir, "resolved", iid+".json")); err != nil || json.Unmarshal(data, &res) != nil || res["outcome"] != "answered" || res["option"] != option {
		t.Fatalf("resolved = %v (%v), want answered %s", res, err, option)
	}

	await(approvalKey(ref, iid) + "/outcome")
	flush()
	edited := false
	for _, evt := range sent {
		edited = edited || evt.Kind == KindEdit && tagValue(evt, "e") == msg.ID.Hex() && strings.Contains(evt.Content, outcome)
	}
	if !edited {
		t.Fatalf("sent = %+v, want the approval message edited with %q", sent, outcome)
	}
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
