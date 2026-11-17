package machineagent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/mailprompt"
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

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
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: id, From: "agent", To: []string{mailboxSender}, Thread: thread, Subject: "re", Created: now.UTC().Format(time.RFC3339Nano), Kind: kind, Refs: []string{prompt}}, Body: body}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if _, err := fsq.DeliverToInboxes(dr, []string{mailboxSender}, id+".md", data); err != nil {
		t.Fatal(err)
	}
	return id
}

// A mailbox binding delivers the DM to the
// handle and ends the turn on its final reply. A status reply is progress
// and does not end the turn (codex 611.36 research, final-answer gap). The
// turn claims its replies like drain (move to cur, drained receipt) and
// leaves other messages in buzz/inbox/new. The prompt carries the
// nostr:<eventID> label, so a layer can fetch the DM event (#1155).
func TestMailboxBindingDeliversAndEndsOnTheFinalReply(t *testing.T) {
	f, b := mailboxRoute(t)
	root := b.Root
	other := replyAs(t, root, cockpitThread(routeSessionPrefix+machineChanA), "another-prompt", format.KindAnswer, "not for this turn")
	f.machine(t)
	in := f.ownerSays(t, machineChanA, "say hi")
	eventID := in.Event.ID.Hex()
	done := f.s.Owner(in)
	id, prompt := promptOf(t, b)
	if !slices.Equal(prompt.Header.Labels, []string{"acp", "buzz", "nostr:" + eventID}) {
		t.Errorf("prompt labels=%q", prompt.Header.Labels)
	}
	replyAs(t, root, prompt.Header.Thread, id, format.KindStatus, "working on it")
	answer := replyAs(t, root, prompt.Header.Thread, id, format.KindAnswer, "hi from the agent")
	waitClosed(t, done)
	if out, _ := f.s.mailboxAnswered(eventID); out.ReplyID != answer || f.posts(machineChanA, "agent: working on it") != 1 {
		t.Fatalf("outcome=%+v posts=%q; want the final reply and the status posted", out, f.agentTexts(machineChanA))
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

// Regression: #957, every poll read every file in
// six folders, so a big mailbox cost a core per open turn. A poll reads only
// buzz/inbox/new; cur is read at the turn start and on the heartbeat.
func TestMailboxRepliesReadOnlyNew(t *testing.T) {
	s, root := standalone(t).s, liveSession(t, "alpha").Root
	thread := cockpitThread("session/s")
	id := replyAs(t, root, thread, "prompt", format.KindAnswer, "already consumed")
	moveToCur(t, root, id)
	watch := s.watchMailbox(binding.Binding{Root: root, Handle: "agent"}, thread, "prompt", time.Now().Add(-time.Minute))
	if _, final, _, err := watch.poll(false); err != nil || final != "" {
		t.Fatalf("final=%q err=%v; want no reply read from cur", final, err)
	}
}

// Regression: #957, two pollers both found
// one reply in new; the one whose move failed with ENOENT also read it from
// cur, so both returned it. Only one poller returns a reply.
func TestMailboxTwoPollersReturnOneReply(t *testing.T) {
	s, root := standalone(t).s, liveSession(t, "alpha").Root
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
	if _, owned, err := second.claimNew(dr, mailprompt.Hit{Filename: id + ".md"}); err != nil || owned {
		t.Fatalf("second poller owns the reply too (err=%v)", err)
	}
	if _, final, _, err := second.poll(true); err != nil || final != "" {
		t.Fatalf("second poller recovered final=%q err=%v", final, err)
	}
}

// Replies that one poll finds together are handled oldest first: each
// status reply is progress, in the order the agent sent them, and the
// answer is the final reply. The replies are in the inbox before the one
// poll, so no timing decides what it sees (review of bead
// agent-message-queue-vosl.9: the order within one poll had no
// deterministic test).
func TestMailboxOnePollReturnsProgressInOrderAndTheFinal(t *testing.T) {
	s, root := standalone(t).s, liveSession(t, "alpha").Root
	thread := cockpitThread("session/s")
	since := time.Now().Add(-time.Minute)
	replyAs(t, root, thread, "prompt", format.KindStatus, "reading the code")
	replyAs(t, root, thread, "prompt", format.KindStatus, "running the tests")
	answer := replyAs(t, root, thread, "prompt", format.KindAnswer, "all done")
	watch := s.watchMailbox(binding.Binding{Root: root, Handle: "agent"}, thread, "prompt", since)
	finalID, final, progress, err := watch.poll(false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"agent: reading the code", "agent: running the tests"}; !slices.Equal(progress, want) || final != "all done" || finalID != answer {
		t.Fatalf("progress=%q final=%q id=%q; want %q, then the answer %s", progress, final, finalID, want, answer)
	}
}

// Regression: #957, the mover emitted
// the drained receipt only if it also won forwarding. When another watch's
// cur recovery won first, no drained receipt was ever written.
func TestMailboxMoverEmitsTheDrainedReceipt(t *testing.T) {
	s, root := standalone(t).s, liveSession(t, "alpha").Root
	thread := cockpitThread("session/s")
	b := binding.Binding{Root: root, Handle: "agent"}
	since := time.Now().Add(-time.Minute)
	id := replyAs(t, root, thread, "prompt", format.KindAnswer, "one answer")
	mover, other := s.watchMailbox(b, thread, "prompt", since), s.watchMailbox(b, thread, "prompt", since)
	if _, _, err := other.recover(id + ".md"); err != nil { // other wins forwarding first
		t.Fatal(err)
	}
	hits, err := mover.inboxNew.Replies("agent", thread, "prompt", since)
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

// Regression: #957, a reply another
// consumer moved to buzz/inbox/cur before the turn saw it was never read, and
// the turn timed out. The turn recovers it, once.
func TestMailboxRecoversAReplyDrainedToCur(t *testing.T) {
	f, b := mailboxRoute(t)
	s, root := f.s, b.Root
	f.machine(t)
	in := f.ownerSays(t, machineChanA, "say hi")
	done := s.Owner(in)
	prompt, msg := promptOf(t, b)
	reply := replyInCur(t, root, msg.Header.Thread, prompt, "drained elsewhere")
	waitClosed(t, done)
	if out, _ := s.mailboxAnswered(in.Event.ID.Hex()); out.ReplyID != reply {
		t.Fatalf("outcome=%+v; want the reply from cur", out)
	}
	watch := s.watchMailbox(binding.Binding{Root: root, Handle: "agent"}, msg.Header.Thread, prompt, time.Now().Add(-time.Minute))
	if _, final, _, err := watch.poll(true); err != nil || final != "" {
		t.Fatalf("recovered again: final=%q err=%v", final, err)
	}
}

// Regression: bead agent-message-queue-611.36, a redelivered event published a second
// message. It must reuse the first delivery's message.
//
// Each turn ends when the client leaves at the delivery update, rather than
// on a 50 ms budget: under parallel load that budget expired before the
// first delivery and nothing was published (seen on #1179's load runs). A
// client that leaves records no event outcome, so the second attempt is a
// real redelivery and must reach delivery too.
//
// On the direct transport the client that leaves is the machine workers
// stopping; the redelivery runs once they start again, and its reply proves
// it reached delivery.
func TestMailboxRedeliveryPublishesOnce(t *testing.T) {
	f, b := mailboxRoute(t)
	root := b.Root
	in := f.ownerSays(t, machineChanA, "say hi")
	eventID := in.Event.ID.Hex()
	stop := f.machine(t)
	first := f.s.Owner(in)
	promptOf(t, b)
	stop()
	if _, decided := f.s.mailboxAnswered(eventID); closed(first) || decided {
		t.Fatal("the first attempt ended or recorded an outcome; want delivered, then the client left")
	}
	f.machine(t)
	second := f.s.Owner(in)
	id, prompt := promptOf(t, b)
	replyAs(t, root, prompt.Header.Thread, id, format.KindAnswer, "hi")
	waitClosed(t, second)
	if f.posts(machineChanA, "hi") != 1 {
		t.Fatalf("posts=%q; want the redelivery to post the reply", f.agentTexts(machineChanA))
	}
	if ids := inboxPrompts(t, root); len(ids) != 1 {
		t.Fatalf("inbox holds %d prompts after a redelivery; want 1", len(ids))
	}
}

// Regression: #951, a `coop exec --session` root has no
// config.json, so `amq reply` to buzz refused with `mailbox for "buzz" is
// incomplete`. Delivery must leave buzz a complete mailbox to answer.
// It calls publishOnce, where the mailbox is made, without a turn timer: a
// 50 ms turn budget expired before delivery on slow CI runners (runs
// 38111619015 and 38141035300), and an expired budget correctly publishes
// nothing.
func TestMailboxDeliveryLeavesBuzzAMailbox(t *testing.T) {
	root := canonicalTempDir(t)
	if err := os.MkdirAll(fsq.AgentInboxNew(root, "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}
	if err := publishOnce(b, "t", strings.Repeat("3", 64), strings.Repeat("e", 64), nil, time.Now(), "say hi"); err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if err := fsq.ValidateExistingMailboxLayout(dr, mailboxSender); err != nil {
		t.Fatal(err)
	}
}

// Regression: #951, a prompt an older amq-acp already delivered is redelivered
// after the upgrade; the redelivery check must not skip creating the buzz
// mailbox its reply needs.
func TestRedeliveredPromptStillGetsTheBuzzMailbox(t *testing.T) {
	root := canonicalTempDir(t)
	id := strings.Repeat("4", 64)
	inbox := fsq.AgentInboxNew(root, "agent")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	// A prompt an older amq-acp delivered: a real message with this id.
	older, err := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: id, From: mailboxSender, To: []string{"agent"},
		Thread: "t", Created: time.Now().UTC().Format(time.RFC3339Nano)}, Body: "say hi"}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, id+".md"), older, 0o600); err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}
	if err := publishOnce(b, "t", id, strings.Repeat("e", 64), nil, time.Now(), "say hi"); err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if err := fsq.ValidateExistingMailboxLayout(dr, mailboxSender); err != nil {
		t.Fatal(err)
	}
}

// Regression: #964, a delivery
// to a deleted handle mailbox recreated it and reported Delivered, and a
// refused delivery could still be published by a later redelivery. It must
// say not delivered, record that as the event's outcome, and never publish
// the event afterwards.
func TestMailboxMissingMailboxIsNotDelivered(t *testing.T) {
	f, b := mailboxRoute(t)
	s, root := f.s, b.Root
	if err := os.RemoveAll(filepath.Join(root, "agents")); err != nil {
		t.Fatal(err)
	}
	f.machine(t)
	in := f.ownerSays(t, machineChanA, "say hi")
	eventID := in.Event.ID.Hex()
	waitClosed(t, s.Owner(in))
	said := f.agentTexts(machineChanA)
	out, _ := s.mailboxAnswered(eventID)
	if !out.NotSubmitted || len(said) != 1 || !strings.HasPrefix(said[0], "Not delivered to agent: ") {
		t.Fatalf("outcome=%+v said=%q", out, said)
	}
	if _, err := os.Lstat(filepath.Join(root, "agents", "agent")); !os.IsNotExist(err) {
		t.Fatalf("the handle mailbox was created: %v", err)
	}
	if _, ok := s.mailboxAnswered(eventID); !ok {
		t.Fatal("the refusal is not the event's outcome")
	}
	// The owner restores the mailbox; a redelivery of the event still
	// publishes nothing.
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, s.Owner(in))
	if ids := inboxPrompts(t, root); len(ids) != 0 {
		t.Fatalf("a redelivery published the refused event: %v", ids)
	}
}

// Regression: #964, a message was delivered, then the handle's
// mailbox was deleted; a redelivery found it absent and recorded "not
// delivered, send it again". Only the attempt that created the claim may.
// The first delivery ends when the machine workers stop, which posts
// nothing, so the redelivery's status is the event's one status post.
func TestMailboxRedeliveryAfterTheMailboxWentIsUncertain(t *testing.T) {
	f, b := mailboxRoute(t)
	s, root := f.s, b.Root
	in := f.ownerSays(t, machineChanA, "say hi")
	eventID := in.Event.ID.Hex()
	stop := f.machine(t)
	s.Owner(in)
	promptOf(t, b)
	stop()
	if err := os.RemoveAll(filepath.Join(root, "agents", "agent")); err != nil {
		t.Fatal(err)
	}
	f.machine(t)
	waitClosed(t, s.Owner(in))
	said := f.agentTexts(machineChanA)
	if len(said) != 1 || !strings.Contains(said[0], "the delivery outcome is unknown") || !strings.Contains(said[0], "amq thread --id") {
		t.Fatalf("said=%q", said)
	}
	if _, ok := s.mailboxAnswered(eventID); ok {
		t.Fatal("a redelivery recorded the delivered event as not delivered")
	}
}

// Regression: #964, a mailbox an older amq made has an inbox but no
// receipts/; delivery completes its layout instead of refusing it.
// It calls publishOnce, where the layout is completed, without a turn timer:
// a 50 ms turn budget expired before delivery under load ("inbox holds 0
// prompts; want 1" in a machine run, the same family as #1050), and an
// expired budget correctly publishes nothing.
func TestMailboxDeliveryCompletesAnOlderMailbox(t *testing.T) {
	root := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fsq.AgentReceipts(root, "agent")); err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}
	if err := publishOnce(b, "t", strings.Repeat("6", 64), strings.Repeat("e", 64), nil, time.Now(), "say hi"); err != nil {
		t.Fatal(err)
	}
	if ids := inboxPrompts(t, root); len(ids) != 1 {
		t.Fatalf("inbox holds %d prompts; want 1", len(ids))
	}
}

// Regression: #895, a turn cancelled before delivery still published. An
// owner turn stops when the machine workers stop, as a client that left;
// the turn runs under the claim Owner wrote for it.
func TestMailboxCancelBeforeDeliveryPublishesNothing(t *testing.T) {
	f, b := mailboxRoute(t)
	s := f.s
	in := f.ownerSays(t, machineChanA, "hello")
	event := in.Event.ID.Hex()
	if _, err := writeClaim(filepath.Join(s.eventDir(), event+".json"), eventClaim{Binding: b, Name: b.Name, Agent: s.id.Agent}); err != nil {
		t.Fatal(err)
	}
	turn := newTurn()
	turn.event = in.Event
	s.mu.Lock()
	s.sessions[routeSessionPrefix+machineChanA] = &sessionState{channel: machineChanA}
	if turn.settleLocked(turnDisconnected) {
		close(turn.done)
	}
	s.mu.Unlock()
	if _, err := s.runBound(routeSessionPrefix+machineChanA, "hello", event, turn); err != nil {
		t.Fatal(err)
	}
	if ids := inboxPrompts(t, b.Root); len(ids) != 0 {
		t.Fatalf("cancelled before delivery, but the inbox has %d prompt(s)", len(ids))
	}
}

// Regression: #895, a cancel while the publish lock was held still
// delivered the prompt once the lock was released. The claim is now created
// inside the locked operation, so the re-check is exercised there. On the
// direct transport a turn stops when the machine workers stop, as a client
// that left.
func TestMailboxCancelWhilePublishLockHeldPublishesNothing(t *testing.T) {
	s, root := standalone(t).s, liveSession(t, "alpha").Root
	turn := newTurn()
	s.mu.Lock()
	if turn.settleLocked(turnDisconnected) {
		close(turn.done)
	}
	s.mu.Unlock()
	r := s.newBoundTurn(strings.Repeat("6", 64), "", turn)
	r.Meta = remoteturn.Meta{Target: "agent"}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}
	claim, err := s.publishClaimed(r, time.Now().Add(time.Second), b, "t", "hello")
	if !errors.Is(err, errStoppedBeforePublish) || claim.MessageID == "" {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	if ids := inboxPrompts(t, root); len(ids) != 0 {
		t.Fatalf("cancelled before publication, but inbox has %d prompt(s)", len(ids))
	}
}

// Regression: #895, an expired turn budget still published when the lock
// was free.
func TestMailboxExpiredBudgetPublishesNothing(t *testing.T) {
	f, b := mailboxRoute(t)
	root := b.Root
	f.s.startup.Timeout = time.Nanosecond
	f.machine(t)
	waitClosed(t, f.s.Owner(f.ownerSays(t, machineChanA, "hello")))
	if ids := inboxPrompts(t, root); len(ids) != 0 {
		t.Fatalf("the turn budget expired before publish, but the inbox has %d prompt(s)", len(ids))
	}
}

// Regression: bead agent-message-queue-611.38, the answer ended
// the ACP turn but never reached the Buzz DM, because buzz-acp does not post
// ACP answer text. amq-acp posts it into the prompt's channel itself.
func TestBuzzAnswerIsPostedIntoTheDMChannel(t *testing.T) {
	f, b := mailboxRoute(t)
	root := b.Root
	f.machine(t)
	in := f.ownerSays(t, machineChanA, "hi")
	done := f.s.Owner(in)
	id, prompt := promptOf(t, b)
	replyAs(t, root, prompt.Header.Thread, id, format.KindStatus, "on it")
	f.waitAgentPosts(t, machineChanA, 1)
	replyAs(t, root, prompt.Header.Thread, id, format.KindAnswer, "hi back")
	waitClosed(t, done)
	posts, state := f.agentTexts(machineChanA), f.s.finalPostState(in.Event.ID.Hex())
	if want := []string{"agent: on it", "hi back"}; !slices.Equal(posts, want) || state != finalPostAccepted {
		t.Fatalf("posts=%q final post %q; want the status reply, then the final", posts, state)
	}
}

// Regression: #959, a redelivered event posted the
// same reply into the Buzz DM a second time. The event posts once.
func TestRedeliveredEventPostsOnce(t *testing.T) {
	f, b := mailboxRoute(t)
	f.machine(t)
	in := f.ownerSays(t, machineChanA, "hi")
	done := f.s.Owner(in)
	id, prompt := promptOf(t, b)
	replyAs(t, b.Root, prompt.Header.Thread, id, format.KindAnswer, "hi back")
	waitClosed(t, done)
	waitClosed(t, f.s.Owner(in))
	if posts := f.agentTexts(machineChanA); len(posts) != 1 {
		t.Fatalf("posts=%q after a redelivery; want 1", posts)
	}
}

// Regression: #961, a reply written after the turn
// timed out never reached the DM. The sweep posts it once and records it.
func TestLateReplyIsPostedOnceAfterTheTurn(t *testing.T) {
	f, b := mailboxRoute(t)
	s, root := f.s, b.Root
	s.startup.Timeout = 300 * time.Millisecond
	stop := f.machine(t)
	in := f.ownerSays(t, machineChanA, "hi")
	eventID := in.Event.ID.Hex()
	done := s.Owner(in)
	id, prompt := promptOf(t, b)
	waitClosed(t, done)
	stop()
	replyAs(t, root, prompt.Header.Thread, id, format.KindAnswer, "late answer")
	for range 2 {
		s.sweepLateReplies(time.Now().Add(time.Hour))
	}
	posts := f.agentTexts(machineChanA)
	if got := strings.Count(strings.Join(posts, "|"), "late answer"); got != 1 {
		t.Fatalf("late answer posted %d times; posts=%q", got, posts)
	}
	if _, err := os.Stat(filepath.Join(s.eventDir(), eventID+".posted.final")); err != nil {
		t.Fatalf("no outcome recorded: %v", err)
	}
}

// Regression: #961, when the sweep had
// posted reply B, a turn that then decided its own reply A lost and still
// showed A. The losing turn posts nothing: the winner, B, stands.
func TestLosingReplyReturnsTheWinner(t *testing.T) {
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	f, root := standalone(t), liveSession(t, "alpha").Root
	s := f.s
	eventID := strings.Repeat("2", 63) + "c"
	winner := replyAs(t, root, cockpitThread("session/s"), "prompt", format.KindAnswer, "reply B")
	moveToCur(t, root, winner)
	if _, won, err := s.decideOutcome(eventID, mailboxOutcome{ReplyID: winner}); err != nil || !won {
		t.Fatalf("won=%v err=%v", won, err)
	}
	turn := newTurn()
	r := s.newBoundTurn(eventID, chanA, turn)
	r.mailbox, r.claimChannel, r.Meta = true, chanA, remoteturn.Meta{Target: "agent"}
	if _, err := s.sayReply(r, "own", "reply A"); err != nil {
		t.Fatal(err)
	}
	if posts := f.agentTexts(chanA); len(posts) != 0 {
		t.Fatalf("posts=%q; want nothing posted", posts)
	}
}

// Regression: #961, a status notice could be
// posted after the event's final text was reserved and posted. A status post
// takes the event's post lock and is skipped once the final marker exists.
func TestStatusNeverPostsAfterTheFinal(t *testing.T) {
	if !lock.AdvisoryLockAvailable() {
		t.Skip("no advisory file lock on this platform")
	}
	const chanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	f := standalone(t)
	s := f.s
	eventID := strings.Repeat("6", 64)
	dir := s.eventDir()
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
	s.publish(chanA, "the answer") // the final path posts after it releases the lock
	select {
	case <-status:
	case <-time.After(3 * time.Second):
		t.Fatal("the status post never finished")
	}
	if got := f.agentTexts(chanA); len(got) != 1 || got[0] != "the answer" {
		t.Fatalf("posts=%q; want only the final", got)
	}
}

// Regression (agent-message-queue-37o.7, finding 8): a damaged final marker
// (truncated JSON, {}, a directory) was read as "the agent already
// answered", so a prompt that was never delivered ended replied/end_turn.
// It still blocks a new delivery, but the outcome is uncertain. On the
// direct transport the marker blocks the event's status post too, so the
// channel shows nothing.
func TestDamagedFinalMarkerIsNeverAReply(t *testing.T) {
	cases := map[string]func(path string) error{
		"truncated-json": func(p string) error { return os.WriteFile(p, []byte(`{"reply_id":"x`), 0o600) },
		"empty-object":   func(p string) error { return os.WriteFile(p, []byte(`{}`), 0o600) },
		"directory":      func(p string) error { return os.Mkdir(p, 0o700) },
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			f, b := mailboxRoute(t)
			s, root := f.s, b.Root
			in := f.ownerSays(t, machineChanA, "hello")
			event := in.Event.ID.Hex()
			if err := os.MkdirAll(s.eventDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := plant(filepath.Join(s.eventDir(), event+".posted.final")); err != nil {
				t.Fatal(err)
			}
			f.machine(t)
			waitClosed(t, s.Owner(in))
			if out, decided := s.mailboxAnswered(event); !decided || out.damaged == nil || out.ReplyID != "" {
				t.Fatalf("outcome=%+v decided=%v; want the damaged marker, never an answer", out, decided)
			}
			if posts := f.agentTexts(machineChanA); len(posts) != 0 {
				t.Fatalf("a damaged marker posted %q", posts)
			}
			if n := len(inboxPrompts(t, root)); n != 0 {
				t.Fatalf("a damaged marker let %d prompts through", n)
			}
		})
	}
}

// Regression (agent-message-queue-37o.7, finding 16): the final marker was
// reserved before the post, so a post that failed before it started (no
// buzz CLI) consumed the answer: neither the sweep nor a redelivery ever
// posted it. Once the CLI is fixed, the next sweep posts it, once. The
// #1149 review found the retry kept the event unsent while its post was in
// flight, so a crash before the post's state was recorded let a later sweep
// post the answer a second time.
//
// On the direct transport a post that never started is one made with no
// relay connection.
func TestUnsentFinalAnswerIsPostedByTheNextSweep(t *testing.T) {
	f, b := mailboxRoute(t)
	s, root := f.s, b.Root
	in := f.ownerSays(t, machineChanA, "hi")
	event := in.Event.ID.Hex()
	var down atomic.Bool
	down.Store(true)
	f.postsOffline(&down, func() {
		// A crash here leaves the disk as it is now; a restarted sweep
		// must not see an unsent answer.
		if c, _ := s.loadLateClaim(event, time.Now().Add(time.Hour)); c != nil && c.unsentReply != "" {
			t.Errorf("while the retry posts, the answer still reads as unsent")
		}
	})
	stop := f.machine(t)
	done := s.Owner(in)
	id, prompt := promptOf(t, b)
	replyAs(t, root, prompt.Header.Thread, id, format.KindAnswer, "the answer")
	waitClosed(t, done)
	stop()
	if posts := f.agentTexts(machineChanA); len(posts) != 0 {
		t.Fatalf("posts with no connection: %q; want none", posts)
	}
	down.Store(false)
	for range 2 {
		s.sweepLateReplies(time.Now().Add(time.Hour))
	}
	if posts := f.agentTexts(machineChanA); len(posts) != 1 || posts[0] != "the answer" {
		t.Fatalf("posts after the connection returned: %q; want the answer once", posts)
	}
}

// Review of the final-post states: a retry that crashed while it posted left
// a lone sending marker that no reader named; it is in doubt, never posted
// again.
func TestFinalPostThatCrashedWhileSendingIsInDoubt(t *testing.T) {
	f, b := mailboxRoute(t)
	s, root := f.s, b.Root
	in := f.ownerSays(t, machineChanA, "hi")
	event := in.Event.ID.Hex()
	var down atomic.Bool
	down.Store(true)
	f.postsOffline(&down, nil)
	stop := f.machine(t)
	done := s.Owner(in)
	id, prompt := promptOf(t, b)
	replyAs(t, root, prompt.Header.Thread, id, format.KindAnswer, "the answer")
	waitClosed(t, done)
	stop()
	if got := s.finalPostState(event); got != finalPostUnsent {
		t.Fatalf("state after a post that never started: %q; want unsent", got)
	}
	if err := os.Rename(s.finalStatePath(event, finalPostUnsent), s.finalStatePath(event, finalPostSending)); err != nil {
		t.Fatal(err)
	}
	down.Store(false)
	for range 2 {
		s.sweepLateReplies(time.Now().Add(time.Hour))
	}
	if got := s.finalPostState(event); got != finalPostSending {
		t.Fatalf("state after a crash while sending: %q; want sending", got)
	}
	if posts := f.agentTexts(machineChanA); len(posts) != 0 {
		t.Fatalf("an in-doubt final answer was posted again: %q", posts)
	}
}
