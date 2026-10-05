package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// mailboxServer binds a binding-mode server to handle "agent" on a fresh
// queue root.
func mailboxServer(t *testing.T) (*Server, string) {
	t.Helper()
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	root := canonicalTempDir(t)
	if err := binding.Write(binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}); err != nil {
		t.Fatal(err)
	}
	return NewServer(Config{RemoteBinding: true, StateDir: canonicalTempDir(t), TurnTimeout: 2 * time.Second, PollInterval: 5 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond}, "test"), root
}

// inboxPrompts lists the prompt ids in the agent's inbox (new and cur).
func inboxPrompts(t *testing.T, root string) []string {
	t.Helper()
	var ids []string
	for _, dir := range []string{fsq.AgentInboxNew(root, "agent"), fsq.AgentInboxCur(root, "agent")} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	return ids
}

// replyAs delivers a reply from the agent to buzz that refs prompt and
// returns its id.
func replyAs(t *testing.T, root, thread, prompt, kind, body string) string {
	t.Helper()
	cfg := Config{Root: root, Me: mailboxSender, To: "agent"}
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: id, From: "agent", To: []string{mailboxSender}, Thread: thread, Subject: "re", Created: now.UTC().Format(time.RFC3339Nano), Kind: kind, Refs: []string{prompt}}, Body: body}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(cfg.Root)
	dr, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if _, err := fsq.DeliverToInboxes(dr, []string{mailboxSender}, id+".md", data); err != nil {
		t.Fatal(err)
	}
	return id
}

// Bead agent-message-queue-611.36: a mailbox binding delivers the DM to the
// handle and ends the turn on its final reply. A status reply is progress
// and does not end the turn (codex 611.36 research, final-answer gap). The
// turn claims its replies like drain (move to cur, drained receipt) and
// leaves other messages in buzz/inbox/new.
func TestMailboxBindingDeliversAndEndsOnTheFinalReply(t *testing.T) {
	s, root := mailboxServer(t)
	other := replyAs(t, root, cockpitThread("session/s"), "another-prompt", format.KindAnswer, "not for this turn")
	var thoughts []string
	var answer string
	replied := false
	result, rpcErr := s.runRemote("s", "say hi", strings.Repeat("1", 64), newTurn(), func(v any) error {
		note := v.(sessionUpdateNotification)
		if note.Params.Update.SessionUpdate == "agent_thought_chunk" {
			thoughts = append(thoughts, note.Params.Update.Content.Text)
		}
		if !replied {
			if ids := inboxPrompts(t, root); len(ids) == 1 {
				replied = true
				replyAs(t, root, cockpitThread("session/s"), ids[0], format.KindStatus, "working on it")
				answer = replyAs(t, root, cockpitThread("session/s"), ids[0], format.KindAnswer, "hi from the agent")
			}
		}
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	got := result.(remotePromptResult)
	if got.StopReason != StopReasonEndTurn || !strings.Contains(strings.Join(thoughts, "|"), "agent: working on it") {
		t.Fatalf("result=%+v thoughts=%q", got, thoughts)
	}
	for _, path := range []string{
		filepath.Join(fsq.AgentInboxCur(root, mailboxSender), answer+".md"),
		filepath.Join(fsq.AgentReceipts(root, mailboxSender), answer+"__"+mailboxSender+"__drained.json"),
		filepath.Join(fsq.AgentInboxNew(root, mailboxSender), other+".md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("want %s: %v", path, err)
		}
	}
}

// moveToCur moves a buzz inbox file to cur, the way a manual drain would.
func moveToCur(t *testing.T, root, id string) {
	t.Helper()
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if err := fsq.MoveNewToCur(dr, mailboxSender, id+".md"); err != nil {
		t.Fatal(err)
	}
}

// Bead agent-message-queue-ee7 (review F2): every poll read every file in
// six folders, so a big mailbox cost a core per open turn. A poll reads only
// buzz/inbox/new; cur is read at the turn start and on the heartbeat.
func TestMailboxRepliesReadOnlyNew(t *testing.T) {
	s, root := mailboxServer(t)
	thread := cockpitThread("session/s")
	id := replyAs(t, root, thread, "prompt", format.KindAnswer, "already consumed")
	moveToCur(t, root, id)
	watch := s.watchMailbox(binding.Binding{Root: root, Handle: "agent"}, thread, "prompt", time.Now().Add(-time.Minute))
	if final, _, err := watch.poll(false); err != nil || final != "" {
		t.Fatalf("final=%q err=%v; want no reply read from cur", final, err)
	}
}

// Bead agent-message-queue-ee7, review of #957 (P1): two pollers both found
// one reply in new; the one whose move failed with ENOENT also read it from
// cur, so both returned it. Only one poller returns a reply.
func TestMailboxTwoPollersReturnOneReply(t *testing.T) {
	s, root := mailboxServer(t)
	thread := cockpitThread("session/s")
	b := binding.Binding{Root: root, Handle: "agent"}
	since := time.Now().Add(-time.Minute)
	id := replyAs(t, root, thread, "prompt", format.KindAnswer, "one answer")
	first, second := s.watchMailbox(b, thread, "prompt", since), s.watchMailbox(b, thread, "prompt", since)
	if final, _, err := first.poll(false); err != nil || final != "one answer" {
		t.Fatalf("first poller: final=%q err=%v", final, err)
	}
	// The second poller listed the reply before the first one moved it.
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if _, owned, err := second.claimNew(dr, id+".md"); err != nil || owned {
		t.Fatalf("second poller owns the reply too (err=%v)", err)
	}
	if final, _, err := second.poll(true); err != nil || final != "" {
		t.Fatalf("second poller recovered final=%q err=%v", final, err)
	}
}

// Bead agent-message-queue-ee7, review of #957 (P2): a reply another
// consumer moved to buzz/inbox/cur before the turn saw it was never read, and
// the turn timed out. The turn recovers it, once.
func TestMailboxRecoversAReplyDrainedToCur(t *testing.T) {
	s, root := mailboxServer(t)
	prompt := ""
	result, rpcErr := s.runRemote("s", "say hi", strings.Repeat("3", 64), newTurn(), func(any) error {
		if ids := inboxPrompts(t, root); prompt == "" && len(ids) == 1 {
			prompt = ids[0]
			moveToCur(t, root, replyAs(t, root, cockpitThread("session/s"), prompt, format.KindAnswer, "drained elsewhere"))
		}
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult); got.StopReason != StopReasonEndTurn {
		t.Fatalf("result=%+v; want the reply from cur", got)
	}
	watch := s.watchMailbox(binding.Binding{Root: root, Handle: "agent"}, cockpitThread("session/s"), prompt, time.Now().Add(-time.Minute))
	if final, _, err := watch.poll(true); err != nil || final != "" {
		t.Fatalf("recovered again: final=%q err=%v", final, err)
	}
}

// Codex 611.36 research, replay gap: a redelivered event published a second
// message. It must reuse the first delivery's message.
func TestMailboxRedeliveryPublishesOnce(t *testing.T) {
	s, root := mailboxServer(t)
	s.cfg.TurnTimeout = 50 * time.Millisecond
	eventID := strings.Repeat("2", 64)
	for range 2 {
		if _, rpcErr := s.runRemote("s", "say hi", eventID, newTurn(), func(any) error { return nil }); rpcErr != nil {
			t.Fatal(rpcErr)
		}
	}
	if ids := inboxPrompts(t, root); len(ids) != 1 {
		t.Fatalf("inbox holds %d prompts after a redelivery; want 1", len(ids))
	}
}

// Codex 611.36 research, honest stop: a cancel says the message stays and
// may still run, and the message is not recalled.
func TestMailboxCancelSaysTheMessageStays(t *testing.T) {
	s, root := mailboxServer(t)
	turn := newTurn()
	var said []string
	result, rpcErr := s.runRemote("s", "say hi", strings.Repeat("3", 64), turn, func(v any) error {
		note := v.(sessionUpdateNotification)
		if note.Params.Update.SessionUpdate == "agent_message_chunk" {
			said = append(said, note.Params.Update.Content.Text)
		}
		s.mu.Lock()
		if turn.settleLocked("session_cancelled") {
			close(turn.done)
		}
		s.mu.Unlock()
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult); got.StopReason != StopReasonCancelled || len(said) != 1 || !strings.Contains(said[0], "may still act") {
		t.Fatalf("result=%+v said=%q", got, said)
	}
	if ids := inboxPrompts(t, root); len(ids) != 1 {
		t.Fatalf("inbox holds %d prompts; a cancel must not recall the message", len(ids))
	}
}

// Codex #895 P1 #1: a redelivery on a new ACP session polled its own thread
// and missed the final answer on the first delivery's thread.
func TestMailboxReplayUsesTheFirstThread(t *testing.T) {
	s, root := mailboxServer(t)
	// Long enough for the publish itself. The budget is checked again just
	// before publish (codex #895 r3); 25ms expired under the parallel suite
	// and the first delivery wrote nothing.
	s.cfg.TurnTimeout = time.Second
	eventID := strings.Repeat("4", 64)
	if _, rpcErr := s.runRemote("first", "hello", eventID, newTurn(), func(any) error { return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	ids := inboxPrompts(t, root)
	if len(ids) != 1 {
		t.Fatalf("first delivery count = %d", len(ids))
	}
	replyAs(t, root, cockpitThread("session/first"), ids[0], format.KindAnswer, "first reply")
	s.cfg.TurnTimeout = time.Second
	said := ""
	result, rpcErr := s.runRemote("second", "hello", eventID, newTurn(), func(v any) error {
		if note := v.(sessionUpdateNotification); note.Params.Update.SessionUpdate == "agent_message_chunk" {
			said += note.Params.Update.Content.Text
		}
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult); got.StopReason != StopReasonEndTurn || said != "first reply" {
		t.Fatalf("replay: stop=%s said=%q; want the first reply", got.StopReason, said)
	}
}

// Codex #895 P1 #3: a turn cancelled before delivery still published.
func TestMailboxCancelBeforeDeliveryPublishesNothing(t *testing.T) {
	s, root := mailboxServer(t)
	turn := newTurn()
	s.mu.Lock()
	if turn.settleLocked("session_cancelled") {
		close(turn.done)
	}
	s.mu.Unlock()
	if _, rpcErr := s.runRemote("s", "hello", strings.Repeat("5", 64), turn, func(any) error { return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if ids := inboxPrompts(t, root); len(ids) != 0 {
		t.Fatalf("cancelled before delivery, but the inbox has %d prompt(s)", len(ids))
	}
}

// Codex #895 r2 P1: a cancel while the publish lock was held still
// delivered the prompt once the lock was released.
func TestMailboxCancelWhilePublishLockHeldPublishesNothing(t *testing.T) {
	s, root := mailboxServer(t)
	eventID := strings.Repeat("6", 64)
	turn := newTurn()
	lockPath := filepath.Join(s.cfg.StateDir, "remote-events", eventID+".mailbox.lock")
	claimPath := filepath.Join(s.cfg.StateDir, "remote-events", eventID+".mailbox.json")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	finished := make(chan *rpcError, 1)
	if err := lock.WithExclusiveFileLock(lockPath, func() error {
		go func() {
			_, rpcErr := s.runRemote("s", "hello", eventID, turn, func(any) error { return nil })
			finished <- rpcErr
		}()
		deadline := time.After(time.Second)
		for {
			if _, err := os.Lstat(claimPath); err == nil {
				break
			}
			select {
			case <-deadline:
				t.Fatal("claim not created")
			default:
				time.Sleep(time.Millisecond)
			}
		}
		if ids := inboxPrompts(t, root); len(ids) != 0 {
			t.Fatalf("message published while lock held")
		}
		s.mu.Lock()
		if turn.settleLocked("session_cancelled") {
			close(turn.done)
		}
		s.mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case rpcErr := <-finished:
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt did not finish")
	}
	if ids := inboxPrompts(t, root); len(ids) != 0 {
		t.Fatalf("cancelled before publication, but inbox has %d prompt(s)", len(ids))
	}
}

// Codex #895 r3 P2: an expired turn budget still published when the lock
// was free.
func TestMailboxExpiredBudgetPublishesNothing(t *testing.T) {
	s, root := mailboxServer(t)
	s.cfg.TurnTimeout = time.Nanosecond
	if _, rpcErr := s.runRemote("s", "hello", strings.Repeat("7", 64), newTurn(), func(any) error { return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if ids := inboxPrompts(t, root); len(ids) != 0 {
		t.Fatalf("the turn budget expired before publish, but the inbox has %d prompt(s)", len(ids))
	}
}

// Bead agent-message-queue-611.38 (live test 2026-09-24): the answer ended
// the ACP turn but never reached the Buzz DM, because buzz-acp does not post
// ACP answer text. amq-acp posts it into the prompt's channel itself.
func TestBuzzAnswerIsPostedIntoTheDMChannel(t *testing.T) {
	s, root := mailboxServer(t)
	type post struct{ channel, content string }
	var posts []post
	saved := postAnswer
	t.Cleanup(func() { postAnswer = saved })
	postAnswer = func(channel, content string) error {
		posts = append(posts, post{channel, content})
		return nil
	}
	prompt := "<context>\nScope: dm\nChannel: DM (#6eff60e4-32ab-48ec-bd3d-f4c97872f370)\n</context>\nhi"
	turn := newTurn()
	turn.channel = buzzChannel(prompt)
	replied := false
	result, rpcErr := s.runRemote("s", prompt, "", turn, func(any) error {
		if !replied {
			if ids := inboxPrompts(t, root); len(ids) == 1 {
				replied = true
				replyAs(t, root, cockpitThread("session/s"), ids[0], format.KindAnswer, "hi back")
			}
		}
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	got := result.(remotePromptResult)
	if len(posts) != 1 || posts[0].channel != "6eff60e4-32ab-48ec-bd3d-f4c97872f370" || posts[0].content != "hi back" || got.Meta.Remote.Posted != "posted" {
		t.Fatalf("posts=%+v meta=%+v", posts, got.Meta.Remote)
	}
}

// Bead agent-message-queue-611.39: two sessions, each with its own named
// binding. Each Buzz agent's model selects its binding, so each prompt lands
// in its own session's inbox.
func TestModelSelectsEachAgentsSession(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	rootA, rootB := canonicalTempDir(t), canonicalTempDir(t)
	for _, b := range []binding.Binding{
		{Carrier: binding.CarrierMailbox, Root: rootA, Handle: "agent", Name: "a"},
		{Carrier: binding.CarrierMailbox, Root: rootB, Handle: "agent", Name: "b"},
	} {
		if err := binding.WriteNamed(b); err != nil {
			t.Fatal(err)
		}
	}
	live := startServer(t, Config{RemoteBinding: true, StateDir: canonicalTempDir(t), TurnTimeout: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond})
	live.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2}}`)
	live.read()
	for i, name := range []string{"a", "b"} {
		live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":"/tmp"}}`, 10+i))
		created := live.read()["result"].(map[string]any)
		if n := len(created["models"].(map[string]any)["availableModels"].([]any)); n != 2 {
			t.Fatalf("session/new advertises %d models; want one per binding", n)
		}
		sid := created["sessionId"].(string)
		live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/set_model","params":{"sessionId":%q,"modelId":"amq-remote:%s"}}`, 20+i, sid, name))
		if reply := live.read(); reply["error"] != nil {
			t.Fatalf("set_model %s: %v", name, reply)
		}
		live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"for %s"}]}}`, 30+i, sid, name))
		live.readUntilResult()
	}
	for name, root := range map[string]string{"a": rootA, "b": rootB} {
		if ids := inboxPrompts(t, root); len(ids) != 1 {
			t.Fatalf("session %s inbox holds %d prompts; want 1", name, len(ids))
		}
	}
}
