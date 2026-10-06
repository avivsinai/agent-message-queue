package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if _, final, _, err := watch.poll(false); err != nil || final != "" {
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
	if _, final, _, err := first.poll(false); err != nil || final != "one answer" {
		t.Fatalf("first poller: final=%q err=%v", final, err)
	}
	// The second poller listed the reply before the first one moved it.
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if _, owned, err := second.claimNew(dr, replyHit{filename: id + ".md"}); err != nil || owned {
		t.Fatalf("second poller owns the reply too (err=%v)", err)
	}
	if _, final, _, err := second.poll(true); err != nil || final != "" {
		t.Fatalf("second poller recovered final=%q err=%v", final, err)
	}
}

// Bead agent-message-queue-ee7, review of #957 r2 (P2): the mover emitted
// the drained receipt only if it also won forwarding. When another watch's
// cur recovery won first, no drained receipt was ever written.
func TestMailboxMoverEmitsTheDrainedReceipt(t *testing.T) {
	s, root := mailboxServer(t)
	thread := cockpitThread("session/s")
	b := binding.Binding{Root: root, Handle: "agent"}
	since := time.Now().Add(-time.Minute)
	id := replyAs(t, root, thread, "prompt", format.KindAnswer, "one answer")
	mover, other := s.watchMailbox(b, thread, "prompt", since), s.watchMailbox(b, thread, "prompt", since)
	if _, _, err := other.recover(id + ".md"); err != nil { // other wins forwarding first
		t.Fatal(err)
	}
	hits, err := mover.inboxNew.replies("agent", thread, "prompt", since)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if _, owned, err := mover.claimNew(dr, hits[0]); err != nil || owned {
		t.Fatalf("owned=%v err=%v; want the move without forwarding", owned, err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentReceipts(root, mailboxSender), id+"__"+mailboxSender+"__drained.json")); err != nil {
		t.Fatalf("no drained receipt: %v", err)
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
	if _, final, _, err := watch.poll(true); err != nil || final != "" {
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

// Bead agent-message-queue-1kc (review F7): a redelivered event posted the
// same reply into the Buzz DM a second time. The event posts once.
func TestRedeliveredEventPostsOnce(t *testing.T) {
	s, root := mailboxServer(t)
	s.cfg.TurnTimeout = 300 * time.Millisecond
	posts := 0
	saved := postAnswer
	t.Cleanup(func() { postAnswer = saved })
	postAnswer = func(string, string) error {
		posts++
		return nil
	}
	prompt := "<context>\nScope: dm\nChannel: DM (#6eff60e4-32ab-48ec-bd3d-f4c97872f370)\n</context>\nhi"
	eventID := strings.Repeat("7", 64)
	replied := false
	for range 2 {
		turn := newTurn()
		turn.channel = buzzChannel(prompt)
		if _, rpcErr := s.runRemote("s", prompt, eventID, turn, func(any) error {
			if !replied {
				if ids := inboxPrompts(t, root); len(ids) == 1 {
					replied = true
					replyAs(t, root, cockpitThread("session/s"), ids[0], format.KindAnswer, "hi back")
				}
			}
			return nil
		}); rpcErr != nil {
			t.Fatal(rpcErr)
		}
	}
	if posts != 1 {
		t.Fatalf("posts=%d after a redelivery; want 1", posts)
	}
}

// Bead agent-message-queue-bdq (review F1): a reply written after the turn
// timed out never reached the DM. The sweep posts it once and records it.
func TestLateReplyIsPostedOnceAfterTheTurn(t *testing.T) {
	s, root := mailboxServer(t)
	s.cfg.TurnTimeout = 300 * time.Millisecond
	var posts []string
	saved := postAnswer
	t.Cleanup(func() { postAnswer = saved })
	postAnswer = func(_, content string) error {
		posts = append(posts, content)
		return nil
	}
	prompt := "<context>\nScope: dm\nChannel: DM (#6eff60e4-32ab-48ec-bd3d-f4c97872f370)\n</context>\nhi"
	eventID := strings.Repeat("8", 64)
	turn := newTurn()
	turn.channel = buzzChannel(prompt)
	if _, rpcErr := s.runRemote("s", prompt, eventID, turn, func(any) error { return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	replyAs(t, root, cockpitThread("session/s"), inboxPrompts(t, root)[0], format.KindAnswer, "late answer")
	for range 2 {
		s.sweepLateReplies(time.Now().Add(time.Hour))
	}
	if got := strings.Count(strings.Join(posts, "|"), "late answer"); got != 1 {
		t.Fatalf("late answer posted %d times; posts=%q", got, posts)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "remote-events", eventID+".posted.final")); err != nil {
		t.Fatalf("no outcome recorded: %v", err)
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

// Bead agent-message-queue-bdq, review of #961 (P1): a redelivery reused
// the claim but posted the answer to its own channel, or with no channel
// recorded the answer without sending it. The event's first channel wins.
func TestRedeliveryPostsTheAnswerToTheFirstChannel(t *testing.T) {
	const chanA, chanB = "6eff60e4-32ab-48ec-bd3d-f4c97872f370", "0b2c39a1-6f1e-4d0a-9b3a-5c1d2e3f4a5b"
	for _, redelivered := range []string{chanB, ""} {
		t.Run("redelivered from "+redelivered, func(t *testing.T) {
			s, root := mailboxServer(t)
			s.cfg.TurnTimeout = 300 * time.Millisecond
			type post struct{ channel, content string }
			var mu sync.Mutex
			var posts []post
			saved := postAnswer
			t.Cleanup(func() { postAnswer = saved })
			postAnswer = func(channel, content string) error {
				mu.Lock()
				defer mu.Unlock()
				posts = append(posts, post{channel, content})
				return nil
			}
			eventID := strings.Repeat("9", 64)
			deliver := func(channel string) {
				turn := newTurn()
				turn.channel = channel
				if _, rpcErr := s.runRemote("s", "hi", eventID, turn, func(any) error { return nil }); rpcErr != nil {
					t.Fatal(rpcErr)
				}
			}
			deliver(chanA) // times out
			replyAs(t, root, cockpitThread("session/s"), inboxPrompts(t, root)[0], format.KindAnswer, "the answer")
			s.cfg.TurnTimeout = time.Minute
			deliver(redelivered)
			mu.Lock()
			defer mu.Unlock()
			var answers []post
			for _, p := range posts {
				if p.content == "the answer" {
					answers = append(answers, p)
				}
			}
			if len(answers) != 1 || answers[0].channel != chanA {
				t.Fatalf("answer posts=%+v; want one, to the first channel", answers)
			}
		})
	}
}

// Bead agent-message-queue-bdq, review of #961 (P2): the sweep checked the
// cancel only before its scan, so a cancel landing between that check and
// the post was posted over. The cancel and the final post share one
// exclusive marker; this is the step that follows the sweep's check.
func TestCancelBeforeTheFinalPostWins(t *testing.T) {
	s, _ := mailboxServer(t)
	posts := 0
	saved := postAnswer
	t.Cleanup(func() { postAnswer = saved })
	postAnswer = func(string, string) error {
		posts++
		return nil
	}
	eventID := strings.Repeat("a", 64)
	if err := s.recordEventCancel(eventID); err != nil {
		t.Fatal(err)
	}
	s.postOnce(eventID, postFinal, []byte(`{"reply_id":"r"}`), "6eff60e4-32ab-48ec-bd3d-f4c97872f370", "late answer")
	if posts != 0 {
		t.Fatalf("posted %d times after a cancel", posts)
	}
}

// parkedTurn runs a mailbox turn for session "s" that the cancel handler can
// find, and parks it once its prompt is delivered until release is closed.
type parkedTurn struct {
	prompt  string
	release chan struct{}
	result  chan remotePromptResult
}

func startParkedTurn(t *testing.T, s *Server, root, eventID, channel string) *parkedTurn {
	t.Helper()
	turn := newTurn()
	turn.channel = channel
	s.mu.Lock()
	s.sessions["s"] = &sessionState{ID: "s", turn: turn}
	s.mu.Unlock()
	p := &parkedTurn{release: make(chan struct{}), result: make(chan remotePromptResult, 1)}
	parked := make(chan string, 1)
	go func() {
		res, rpcErr := s.runRemote("s", "hi", eventID, turn, func(v any) error {
			if strings.HasPrefix(v.(sessionUpdateNotification).Params.Update.Content.Text, "Delivered to") {
				parked <- inboxPrompts(t, root)[0]
				<-p.release
			}
			return nil
		})
		if rpcErr != nil {
			t.Error(rpcErr)
		}
		p.result <- res.(remotePromptResult)
	}()
	select {
	case p.prompt = <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("the turn never delivered its prompt")
	}
	return p
}

func (p *parkedTurn) finish(t *testing.T) remotePromptResult {
	t.Helper()
	close(p.release)
	select {
	case res := <-p.result:
		return res
	case <-time.After(3 * time.Second):
		t.Fatal("the turn did not end")
	}
	return remotePromptResult{}
}

// recordPosts swaps the Buzz poster for a recorder.
func recordPosts(t *testing.T) func() []string {
	var mu sync.Mutex
	var posts []string
	saved := postAnswer
	t.Cleanup(func() { postAnswer = saved })
	postAnswer = func(channel, content string) error {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, channel+": "+content)
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), posts...)
	}
}

// Bead agent-message-queue-bdq, review of #961 r2: a cancel was accepted in
// memory before it was recorded, so a sweep could post in between, and it
// wrote .cancelled even when a reply had already won. The cancel handler
// now decides the event's one durable outcome before it settles the turn.
func TestCancelDecidesTheEventOutcome(t *testing.T) {
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	cancel := json.RawMessage(`{"sessionId":"s"}`)
	t.Run("cancel first", func(t *testing.T) {
		s, root := mailboxServer(t)
		posts := recordPosts(t)
		eventID := strings.Repeat("1", 63) + "a"
		p := startParkedTurn(t, s, root, eventID, chanA)
		if _, rpcErr := s.cancel(cancel); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		if out, ok := s.mailboxAnswered(eventID); !ok || !out.Cancelled {
			t.Fatalf("outcome=%+v recorded=%v when the cancel was accepted; want cancelled", out, ok)
		}
		if res := p.finish(t); res.StopReason != StopReasonCancelled {
			t.Fatalf("result=%+v; want cancelled", res)
		}
		replyAs(t, root, cockpitThread("session/s"), p.prompt, format.KindAnswer, "too late")
		s.sweepLateReplies(time.Now().Add(time.Hour))
		if got := strings.Join(posts(), "|"); strings.Contains(got, "too late") {
			t.Fatalf("posted after the cancel: %q", got)
		}
	})
	t.Run("reply first", func(t *testing.T) {
		s, root := mailboxServer(t)
		posts := recordPosts(t)
		eventID := strings.Repeat("1", 63) + "b"
		p := startParkedTurn(t, s, root, eventID, chanA)
		replyAs(t, root, cockpitThread("session/s"), p.prompt, format.KindAnswer, "the answer")
		s.sweepLateReplies(time.Now().Add(time.Hour))
		if _, rpcErr := s.cancel(cancel); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		if res := p.finish(t); res.StopReason != StopReasonEndTurn {
			t.Fatalf("result=%+v; want the answer", res)
		}
		if s.eventCancelled(eventID) {
			t.Fatal(".cancelled written although the reply won")
		}
		if got := posts(); len(got) != 1 || got[0] != chanA+": the answer" {
			t.Fatalf("posts=%q; want the answer once", got)
		}
	})
}

// Bead agent-message-queue-bdq, review of #961 r2 (P1): a turn with no DM
// channel consumed the final-answer record without a post, so the sweep
// never posted the reply once a later delivery named a channel.
func TestReplyWithoutAChannelWaitsForOne(t *testing.T) {
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	s, root := mailboxServer(t)
	posts := recordPosts(t)
	eventID := strings.Repeat("1", 63) + "c"
	p := startParkedTurn(t, s, root, eventID, "")
	replyAs(t, root, cockpitThread("session/s"), p.prompt, format.KindAnswer, "the answer")
	if res := p.finish(t); res.StopReason != StopReasonEndTurn || len(posts()) != 0 {
		t.Fatalf("result=%+v posts=%q; want the answer to the client only", res, posts())
	}
	s.eventChannel(eventID, "", chanA) // a later delivery names the DM
	s.sweepLateReplies(time.Now().Add(time.Hour))
	if got := posts(); len(got) != 1 || got[0] != chanA+": the answer" {
		t.Fatalf("posts=%q; want the answer once, to the DM", got)
	}
}

// Bead agent-message-queue-bdq, review of #961 r3 (P1): the turn won the
// event's outcome, then a failed ACP emission returned before the Buzz
// post, and nothing ever posted the reply. The post comes first.
func TestFailedEmissionStillPostsTheReply(t *testing.T) {
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	s, root := mailboxServer(t)
	posts := recordPosts(t)
	turn := newTurn()
	turn.channel = chanA
	replied := false
	_, rpcErr := s.runRemote("s", "hi", strings.Repeat("2", 63)+"a", turn, func(v any) error {
		update := v.(sessionUpdateNotification).Params.Update
		if update.SessionUpdate == "agent_message_chunk" {
			return fmt.Errorf("client gone")
		}
		if ids := inboxPrompts(t, root); !replied && len(ids) == 1 {
			replied = true
			replyAs(t, root, cockpitThread("session/s"), ids[0], format.KindAnswer, "the answer")
		}
		return nil
	})
	if rpcErr == nil {
		t.Fatal("want the emission error")
	}
	if got := posts(); len(got) != 1 || got[0] != chanA+": the answer" {
		t.Fatalf("posts=%q; want the answer posted once", got)
	}
}

// Bead agent-message-queue-bdq, review of #961 r3 (P2): a cancel that
// arrived before the turn loaded its claim settled only in memory, so a
// sweep could still post the event's late reply.
func TestEarlyCancelIsDecidedBeforeTheSweep(t *testing.T) {
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	s, root := mailboxServer(t)
	s.cfg.TurnTimeout = 300 * time.Millisecond
	posts := recordPosts(t)
	eventID := strings.Repeat("2", 63) + "b"
	prompt := "<context>\nScope: dm\nChannel: DM (#" + chanA + ")\n</context>\nhi"
	first := newTurn()
	first.channel = chanA
	if _, rpcErr := s.runRemote("s", prompt, eventID, first, func(any) error { return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	replyAs(t, root, cockpitThread("session/s"), inboxPrompts(t, root)[0], format.KindAnswer, "the answer")
	s.mu.Lock()
	s.ready = true
	s.sessions["s"] = &sessionState{ID: "s"}
	s.mu.Unlock()
	run, _, rpcErr := s.beginPrompt(json.RawMessage(`{"sessionId":"s","prompt":[{"type":"text","text":` + mustJSONString(t, prompt) + `}],"_meta":{"nostr":{"eventId":"` + eventID + `"}}}`))
	if rpcErr != nil || run == nil {
		t.Fatalf("begin prompt: %v", rpcErr)
	}
	if _, rpcErr := s.cancel(json.RawMessage(`{"sessionId":"s"}`)); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	s.sweepLateReplies(time.Now().Add(time.Hour))
	if got := strings.Join(posts(), "|"); strings.Contains(got, "the answer") {
		t.Fatalf("posted after an accepted cancel: %q", got)
	}
	if res, rpcErr := run(func(any) error { return nil }); rpcErr != nil || res.(remotePromptResult).StopReason != StopReasonCancelled {
		t.Fatalf("result=%+v err=%v; want cancelled", res, rpcErr)
	}
}

// Bead agent-message-queue-bdq, review of #961 r3 (P2): when the sweep had
// posted reply B, a turn that then decided its own reply A lost and still
// showed A. It returns the winner, B.
func TestLosingReplyReturnsTheWinner(t *testing.T) {
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	s, root := mailboxServer(t)
	posts := recordPosts(t)
	eventID := strings.Repeat("2", 63) + "c"
	winner := replyAs(t, root, cockpitThread("session/s"), "prompt", format.KindAnswer, "reply B")
	moveToCur(t, root, winner)
	if _, won, err := s.decideOutcome(eventID, mailboxOutcome{ReplyID: winner}); err != nil || !won {
		t.Fatalf("won=%v err=%v", won, err)
	}
	var shown []string
	turn := newTurn()
	turn.channel = chanA
	r := &remoteTurn{s: s, eventID: eventID, turn: turn, mailbox: true, claimChannel: chanA, meta: remoteMeta{Target: "agent"}, emit: func(v any) error {
		shown = append(shown, v.(sessionUpdateNotification).Params.Update.Content.Text)
		return nil
	}}
	if _, rpcErr := s.sayReply(r, binding.Binding{Root: root, Handle: "agent"}, "own", "reply A"); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if strings.Join(shown, "|") != "reply B" || len(posts()) != 0 {
		t.Fatalf("shown=%q posts=%q; want reply B shown and nothing posted", shown, posts())
	}
}

// Bead agent-message-queue-iqh (Ben review F9): a session whose model names a
// missing binding must not fall back to the only binding left.
func TestMissingModelNeverReachesAnotherBinding(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	rootB := canonicalTempDir(t)
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: rootB, Handle: "agent", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	live := startServer(t, Config{RemoteBinding: true, StateDir: canonicalTempDir(t), TurnTimeout: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond})
	live.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2}}`)
	live.read()
	live.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`)
	sid := live.read()["result"].(map[string]any)["sessionId"].(string)
	live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/set_model","params":{"sessionId":%q,"modelId":"amq-remote:a"}}`, sid))
	if reply := live.read(); reply["error"] == nil {
		t.Fatalf("set_model accepted a missing binding: %v", reply)
	}
	live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"for a"}]}}`, sid))
	live.readUntilResult()
	if ids := inboxPrompts(t, rootB); len(ids) != 0 {
		t.Fatalf("a prompt for missing binding a reached binding b: %d prompts", len(ids))
	}
}

// Bead agent-message-queue-iqh, Pro review of #954: the model session/new
// advertises pins the session, so removing that binding never falls back to
// the one that remains.
func TestAdvertisedModelPinsSessionWithoutSetModel(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	rootA, rootB := canonicalTempDir(t), canonicalTempDir(t)
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: rootA, Handle: "agent", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	live := startServer(t, Config{RemoteBinding: true, StateDir: canonicalTempDir(t), TurnTimeout: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond})
	live.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2}}`)
	live.read()
	live.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`)
	sid := live.read()["result"].(map[string]any)["sessionId"].(string)
	if _, err := binding.RemoveMatching(func(b binding.Binding) bool { return b.Name == "a" }); err != nil {
		t.Fatal(err)
	}
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: rootB, Handle: "agent", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"for a"}]}}`, sid))
	live.readUntilResult()
	if ids := inboxPrompts(t, rootB); len(ids) != 0 {
		t.Fatalf("a session advertised as a reached binding b: %d prompts", len(ids))
	}
}

// Review of #957 r4 (agent-message-queue-bdq): another turn or the sweep
// decided the event's outcome after this turn's last loop check, and the
// turn still timed out and posted a timeout notice. It adopts the outcome.
func TestDeadlineAdoptsAnOutcomeDecidedMeanwhile(t *testing.T) {
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	s, root := mailboxServer(t)
	s.cfg.TurnTimeout = 300 * time.Millisecond
	s.cfg.PollInterval, s.cfg.HeartbeatInterval = time.Hour, time.Hour // only the deadline wakes the loop
	posts := recordPosts(t)
	eventID := strings.Repeat("3", 64)
	thread := cockpitThread("session/s")
	var shown []string
	turn := newTurn()
	turn.channel = chanA
	_, rpcErr := s.runRemote("s", "hi", eventID, turn, func(v any) error {
		text := v.(sessionUpdateNotification).Params.Update.Content.Text
		shown = append(shown, text)
		switch {
		case strings.HasPrefix(text, "Delivered to"):
			replyAs(t, root, thread, inboxPrompts(t, root)[0], format.KindStatus, "working")
		case text == "agent: working":
			// After the loop's check: the other turn decides reply B, then
			// this turn's deadline passes.
			winner := replyAs(t, root, thread, "other", format.KindAnswer, "reply B")
			moveToCur(t, root, winner)
			if _, _, err := s.decideOutcome(eventID, mailboxOutcome{ReplyID: winner}); err != nil {
				t.Error(err)
			}
			time.Sleep(400 * time.Millisecond)
		}
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := posts(); len(got) != 0 {
		t.Fatalf("posted to the DM: %q", got)
	}
	if last := shown[len(shown)-1]; last != "reply B" {
		t.Fatalf("shown=%q; want the adopted reply B", shown)
	}
}

// Review of #961 r7 (agent-message-queue-bdq): a status notice could be
// posted after the event's final text was reserved and posted. A status post
// takes the event's post lock and is skipped once the final marker exists.
func TestStatusNeverPostsAfterTheFinal(t *testing.T) {
	if !lock.AdvisoryLockAvailable() {
		t.Skip("no advisory file lock on this platform")
	}
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	s, _ := mailboxServer(t)
	posts := recordPosts(t)
	eventID := strings.Repeat("6", 64)
	dir := filepath.Join(s.cfg.StateDir, "remote-events")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	status := make(chan string, 1)
	err := lock.WithExclusiveFileLock(filepath.Join(dir, eventID+".post.lock"), func() error {
		go func() { status <- s.postOnce(eventID, postStatus, []byte("x\n"), chanA, "No final reply yet") }()
		_, err := createExclusive(filepath.Join(dir, eventID+".posted."+postFinal), []byte(`{"reply_id":"r"}`))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	publish(chanA, "the answer") // the final path posts after it releases the lock
	select {
	case <-status:
	case <-time.After(3 * time.Second):
		t.Fatal("the status post never finished")
	}
	if got := posts(); len(got) != 1 || got[0] != chanA+": the answer" {
		t.Fatalf("posts=%q; want only the final", got)
	}
}
