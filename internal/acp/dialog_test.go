package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
)

// The machine agent posts each open dialog of its session into its channel once.
// The owner's ✅ answers option 1, and the resolution edits the post. A
// typed "3" answers the next dialog and never reaches the session.
func TestDialogTickPostsReactsEdits(t *testing.T) {
	f := standalone(t)
	b := liveSession(t, "alpha")
	b.Label = "Claude Code · proj @ mac"
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, f.s, b, machineChanA)
	f.groupState(t, machineChanA)
	dir, err := dialog.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	options := []dialog.Option{{N: 1, Label: "Allow once", Value: "allow"}, {N: 2, Label: "Allow this session", Value: "session"}, {N: 3, Label: "Deny", Value: "deny", Deny: true}}
	first := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", Text: "$ ls /tmp", Options: options, OpenedAt: time.Now()}
	if err := dialog.Open(dir, first); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f.s.dialogTick(now)
	msgs := f.agentMessages(machineChanA)
	want := "Claude Code · proj @ mac wants to run Bash:\n$ ls /tmp\n\n1. Allow once\n2. Allow this session\n3. Deny\n\nReact ✅ = 1, ❌ = Deny, or reply a number / yes / no."
	if len(msgs) != 1 || msgs[0].Content != want {
		t.Fatalf("posts = %+v; want the dialog once", msgs)
	}
	f.ownerReacts(t, machineChanA, msgs[0].ID, "✅")
	f.s.dialogTick(now)
	if _, _, a, _, _ := dialog.Lookup(dir, first.ID); a == nil || a.Option != 1 {
		t.Fatalf("answer = %+v; want option 1", a)
	}
	if _, err := dialog.Resolve(dir, dialog.Resolved{ID: first.ID, Outcome: dialog.OutcomeAllowed, Label: "Allow once", By: dialog.ByBuzz, At: now}); err != nil {
		t.Fatal(err)
	}
	f.s.dialogTick(now)
	f.s.dialogTick(now)
	if edits := f.agentEdits(machineChanA, msgs[0].ID); len(edits) != 1 || edits[0].Content != want+"\n\n→ Allow once (Buzz)" {
		t.Fatalf("edits = %+v; want the resolution once", edits)
	}

	second := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Edit", Text: "main.go", Options: options, OpenedAt: time.Now()}
	if err := dialog.Open(dir, second); err != nil {
		t.Fatal(err)
	}
	f.s.dialogTick(now)
	f.startMachine(t)
	waitClosed(t, f.s.Owner(f.ownerSays(t, machineChanA, "3")))
	if _, _, a, _, _ := dialog.Lookup(dir, second.ID); a == nil || a.Option != 3 {
		t.Fatalf("answer = %+v; want option 3", a)
	}
	if got := inboxPrompts(t, b.Root); len(got) != 0 {
		t.Fatalf("the answer reached the session: %v", got)
	}
	if last := f.lastAgentPost(machineChanA); last != "Answered." {
		t.Fatalf("posts end with %q; want the answer acknowledged", last)
	}
}

func TestOutcomeLineShowsWhyADialogExpired(t *testing.T) {
	for _, row := range []struct{ label, want string }{
		{"the asking agent ended", "→ Expired: the asking agent ended"},
		{"", "→ Expired"},
	} {
		if got := outcomeLine(dialog.Resolved{Outcome: dialog.OutcomeExpired, Label: row.label, By: dialog.ByTimeout}); got != row.want {
			t.Fatalf("outcomeLine(label %q) = %q; want %q", row.label, got, row.want)
		}
	}
}

// Regression (idle profile of a machine agent): every open dialog, hours
// old or new, read its reactions every 3 s, oldest first, so stale dialogs
// spawned a process each and delayed a new one. An old dialog backs off; a
// fresh one is read at 3 s, and first.
func TestDialogReactionReadsBackOffByAgeNewestFirst(t *testing.T) {
	var reads []string
	var f *standaloneFixture
	f = standalone(t, func(r *relaytest.Relay) {
		r.BeforeReq = func(filters []nostr.Filter) {
			for _, filter := range filters {
				for _, id := range filter.Tags["e"] {
					for _, post := range f.agentMessages(machineChanA) {
						if post.ID.Hex() == id {
							reads = append(reads, strings.Fields(strings.SplitN(post.Content, "\n", 2)[1])[1])
							return
						}
					}
				}
			}
		}
	})
	b := liveSession(t, "alpha")
	activeChannel(t, f.s, b, machineChanA)
	f.groupState(t, machineChanA)
	dir, err := dialog.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, d := range []struct {
		text string
		age  time.Duration
	}{{"$ old", 30 * time.Minute}, {"$ new", 0}} {
		if err := dialog.Open(dir, dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", Text: d.text, Options: []dialog.Option{{N: 1, Label: "Allow once", Value: "allow"}}, OpenedAt: now.Add(-d.age)}); err != nil {
			t.Fatal(err)
		}
	}
	f.s.dialogTick(now)
	f.s.dialogTick(now)
	if strings.Join(reads, ",") != "new,old" {
		t.Fatalf("first reads = %v", reads)
	}
	reads = nil
	f.s.dialogTick(now.Add(reactionPollInterval))
	if strings.Join(reads, ",") != "new" {
		t.Fatalf("reads after %v = %v; want only new", reactionPollInterval, reads)
	}
	reads = nil
	f.s.dialogTick(now.Add(reactionPollMax))
	if strings.Join(reads, ",") != "new,old" {
		t.Fatalf("reads after %v = %v; want new,old at cap", reactionPollMax, reads)
	}
}

// Regression (idle profile of a machine agent): a post edit that failed
// was tried again on every 2 s tick, one buzz process each, for about 20
// minutes. A failed edit now waits 4 s, then 8 s; the first is immediate.
func TestDialogFailedEditBacksOff(t *testing.T) {
	f := standalone(t, func(r *relaytest.Relay) { r.RejectKind.Store(40003) })
	b := liveSession(t, "alpha")
	activeChannel(t, f.s, b, machineChanA)
	f.groupState(t, machineChanA)
	dir, err := dialog.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	openPostedDialog(t, f.s, dir, "1")
	requests, err := dialog.Requests(dir)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests = %+v, %v", requests, err)
	}
	req := requests[0]
	if _, err := dialog.Resolve(dir, dialog.Resolved{ID: req.ID, Outcome: dialog.OutcomeTerminal, By: dialog.ByTerminal, At: now}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		after time.Duration
		edits int32
	}{{0, 1}, {2 * time.Second, 1}, {dialogEditBackoffMin, 2}, {dialogEditBackoffMin + 2*time.Second, 2}, {3 * dialogEditBackoffMin, 3}} {
		f.s.dialogTick(now.Add(step.after))
		if got := f.relay.Rejected.Load(); got != step.edits {
			t.Fatalf("publisher attempts after %v = %d; want %d", step.after, got, step.edits)
		}
	}
}

// A free-text dialog takes only the owner's typed answer, with all of its
// line breaks; a reaction answers nothing.
func TestFreeTextDialogTakesOnlyTheOwnersTypedAnswer(t *testing.T) {
	f, dir := ownerDialogServer(t)
	req := dialog.Request{ID: dialog.NewID(), Harness: "codex", Text: "Which database?\nMigrate now?", FreeText: true, OpenedAt: time.Now()}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	f.s.dialogTick(time.Now())
	msgs := f.agentMessages(machineChanA)
	f.ownerReacts(t, machineChanA, msgs[len(msgs)-1].ID, "✅")
	f.ownerReacts(t, machineChanA, msgs[len(msgs)-1].ID, "❌")
	f.s.dialogTick(time.Now())
	if _, _, a, _, _ := dialog.Lookup(dir, req.ID); a != nil {
		t.Fatalf("a reaction answered a free-text dialog: %+v", a)
	}
	f.startMachine(t)
	waitClosed(t, f.s.Owner(f.ownerSays(t, machineChanA, "postgres\nyes")))
	if _, _, a, _, _ := dialog.Lookup(dir, req.ID); a == nil || a.Text != "postgres\nyes" || a.Event == "" {
		t.Fatalf("owner answer = %+v; want the two lines bound to their event", a)
	}
}

// An owner event before its dialog never answers it. One fresh answered
// event stays bound: it cannot answer the dialog that follows.
func TestOwnerAnswerMustBeNewerThanItsDialogAndUnused(t *testing.T) {
	f, dir := ownerDialogServer(t)
	old := f.ownerSignedAt(t, machineChanA, "yes", time.Now().Add(-time.Hour))
	first := openPostedDialog(t, f.s, dir, "1")
	if _, a, ok := f.s.dialogReplyOf(f.ownerMsg(machineChanA, old)); ok {
		t.Fatalf("an hour-old owner event answered a new dialog: %+v", a)
	}
	used := f.ownerSignedAt(t, machineChanA, "yes", time.Now())
	_, a, ok := f.s.dialogReplyOf(f.ownerMsg(machineChanA, used))
	if !ok {
		t.Fatal("a fresh owner yes did not answer the only open dialog")
	}
	if _, err := dialog.WriteAnswer(dir, a); err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.Resolve(dir, dialog.Resolved{ID: first.ID, Outcome: dialog.OutcomeAllowed, By: dialog.ByBuzz, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	openPostedDialog(t, f.s, dir, "2")
	if _, a, ok := f.s.dialogReplyOf(f.ownerMsg(machineChanA, used)); ok {
		t.Fatalf("the event that answered one dialog answered the next: %+v", a)
	}
}

// Only a NIP-10 root or reply names the dialog it answers, consistently
// with actual relay events. A mere mention or two parent tags picks none.
func TestOwnerAnswerTargetsTheOneDialogItRepliesTo(t *testing.T) {
	cases := []struct {
		name string
		tags func([]nostr.Event) []nostr.Tag
		want int
	}{
		{name: "mention", tags: func(posts []nostr.Event) []nostr.Tag {
			unrelated := strings.Repeat("3", 64)
			return []nostr.Tag{{"e", unrelated, "", "root"}, {"e", unrelated, "", "reply"}, {"e", posts[0].ID.Hex(), "", "mention"}}
		}},
		{name: "both dialogs", tags: func(posts []nostr.Event) []nostr.Tag {
			return []nostr.Tag{{"e", posts[0].ID.Hex(), "", "root"}, {"e", posts[1].ID.Hex(), "", "reply"}}
		}},
		{name: "reply to the second", tags: func(posts []nostr.Event) []nostr.Tag {
			return []nostr.Tag{{"e", posts[1].ID.Hex(), "", "root"}, {"e", posts[1].ID.Hex(), "", "reply"}}
		}, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, dir := ownerDialogServer(t)
			reqs := []dialog.Request{openPostedDialog(t, f.s, dir, "1"), openPostedDialog(t, f.s, dir, "2")}
			evt := f.ownerSignedAt(t, machineChanA, "yes", time.Now(), tc.tags(f.agentMessages(machineChanA))...)
			_, a, ok := f.s.dialogReplyOf(f.ownerMsg(machineChanA, evt))
			if tc.want == 0 && ok || tc.want != 0 && (!ok || a.ID != reqs[tc.want-1].ID) {
				t.Fatalf("dialogReply = %+v, %v; want dialog %d", a, ok, tc.want)
			}
		})
	}
}

// An hour-old signed "mirror on" did not undo the later off: a Claude
// binding that serves that channel is the fixture's real evidence.
func TestOldMirrorOnDoesNotUndoALaterOff(t *testing.T) {
	f, dir := ownerDialogServer(t)
	old := f.ownerSignedAt(t, machineChanA, "mirror on", time.Now().Add(-time.Hour))
	if err := mirror.SetOff("alpha", true); err != nil {
		t.Fatal(err)
	}
	f.startMachine(t)
	waitClosed(t, f.s.Owner(buzzop.Inbound{Channel: machineChanA, Event: old}))
	if off, err := mirror.DirFor("alpha"); err != nil || !mirror.Off(off) {
		t.Fatalf("an hour-old mirror on undid the later mirror off: dir=%s err=%v", dir, err)
	}
}

// The direct event claim preserves the old pending-prompt safety rule: after
// this event was chosen, replaying it does not make another dialog answer.
func TestPendingPromptEventDoesNotMoveToTheNextDialog(t *testing.T) {
	f, dir := ownerDialogServer(t)
	first := openPostedDialog(t, f.s, dir, "1")
	evt := f.ownerSignedAt(t, machineChanA, "yes", time.Now())
	if _, a, ok := f.s.dialogReplyOf(f.ownerMsg(machineChanA, evt)); !ok {
		t.Fatal("the prompt did not choose its dialog")
	} else if _, err := dialog.WriteAnswer(dir, a); err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.Resolve(dir, dialog.Resolved{ID: first.ID, Outcome: dialog.OutcomeDenied, By: dialog.ByTerminal, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	second := openPostedDialog(t, f.s, dir, "2")
	if _, a, ok := f.s.dialogReplyOf(f.ownerMsg(machineChanA, evt)); ok {
		t.Fatalf("the event chosen for the first dialog answered the second: %+v (dialog %v)", a, second.ID)
	}
}

// A recordless off marker still wins over an hour-old "mirror on".
func TestOldMirrorOnDoesNotUndoARecordlessOff(t *testing.T) {
	f, dir := ownerDialogServer(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "off"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := f.ownerSignedAt(t, machineChanA, "mirror on", time.Now().Add(-time.Hour))
	f.startMachine(t)
	waitClosed(t, f.s.Owner(buzzop.Inbound{Channel: machineChanA, Event: old}))
	if !mirror.Off(dir) {
		t.Fatal("an hour-old mirror on undid an off marker with no recorded switch time")
	}
}

// ownerDialogServer makes the direct channel one dialog of alpha can
// answer. The fixture feeds real relay state and owner signatures.
func ownerDialogServer(t *testing.T) (*standaloneFixture, string) {
	f := standalone(t)
	b := liveSession(t, "alpha")
	b.Harness = harness.Claude
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, f.s, b, machineChanA)
	f.groupState(t, machineChanA)
	dir, err := dialog.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	return f, dir
}

// openPostedDialog opens a two-option dialog now and asks the direct relay
// to give it its genuine, unpredictable event id before readbacks.
func openPostedDialog(t *testing.T, s *Server, dir, text string) dialog.Request {
	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", Text: text, OpenedAt: time.Now(),
		Options: []dialog.Option{{N: 1, Label: "Allow once"}, {N: 2, Label: "Deny", Deny: true}}}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	s.dialogTick(time.Now())
	return req
}

// Regression (review of the dialog poster): the dialogs of a gone binding
// were expired and removed in one tick, so the seam still waiting never read
// its resolution. The directory stays while the seam runs, for seamGrace.
func TestGoneBindingKeepsDialogsUntilTheSeamLeaves(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	s := standalone(t).s
	dir, err := dialog.DirFor("gone")
	if err != nil {
		t.Fatal(err)
	}
	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", OpenedAt: time.Now(), PID: os.Getpid()}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.dropGoneDialogs(nil, now)
	if res, ok, _ := dialog.ReadResolved(dir, req.ID); !ok || res.Outcome != dialog.OutcomeExpired {
		t.Fatalf("resolution = %+v, %v; want expired", res, ok)
	}
	s.dropGoneDialogs(nil, now)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the dialogs went while the seam runs: %v", err)
	}
	s.dropGoneDialogs(nil, now.Add(seamGrace+time.Second))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the dialogs stayed after seamGrace: %v", err)
	}
}

// Regression (agent-message-queue-udg review): dropGoneDialogs judged each
// name from the snapshot it took before a loop of relay calls. Session bbb
// attached again during aaa's edit, and its open dialog was expired as the
// dialog of an ended session.
func TestDropGoneDialogsKeepsTheDialogOfASessionThatAttachedAgain(t *testing.T) {
	var attached bool
	liveRoot := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(liveRoot, "agent"); err != nil {
		t.Fatal(err)
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	old := binding.Binding{Carrier: binding.CarrierMailbox, Root: filepath.Join(liveRoot, "gone"), Handle: "agent", Holder: holder, Name: "bbb", Label: "old"}
	f := standalone(t, func(r *relaytest.Relay) {
		r.BeforePublish = func(evt nostr.Event) {
			if attached || evt.Kind != 40003 {
				return
			}
			attached = true
			nb := old
			nb.Root = liveRoot
			if err := binding.WriteNamed(nb); err != nil {
				t.Fatal(err)
			}
		}
	})
	s := f.s
	activeChannel(t, s, old, machineChanA)
	f.groupState(t, machineChanA)
	aDir, _ := dialog.DirFor("aaa")
	aReq := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", OpenedAt: time.Now()}
	if err := dialog.Open(aDir, aReq); err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.Resolve(aDir, dialog.Resolved{ID: aReq.ID, Outcome: dialog.OutcomeExpired, By: dialog.ByTimeout, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	res := s.direct.Send(context.Background(), buzzop.Message{Key: "direct/dialog-post/aaa", Channel: machineChanA, Content: "t"})
	if res.EventID == "" {
		t.Fatalf("direct post outcome = %s", res.Outcome)
	}
	postID := nostr.MustIDFromHex(res.EventID)
	raw, _ := json.Marshal(dialogPost{Channel: machineChanA, Event: postID.Hex(), Text: "t"})
	if _, err := fsq.WriteFileAtomicFollowLinks(s.dialogPostDir("aaa"), aReq.ID+".json", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	bDir, _ := dialog.DirFor("bbb")
	bReq := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", OpenedAt: time.Now(), PID: os.Getpid()}
	if err := dialog.Open(bDir, bReq); err != nil {
		t.Fatal(err)
	}
	s.dropGoneDialogs(nil, time.Now())
	if res, ok, _ := dialog.ReadResolved(bDir, bReq.ID); ok {
		t.Fatalf("the open dialog of live session bbb was resolved: %+v", res)
	}
	if !attached {
		t.Fatal("the real closing edit was not published")
	}
}
