package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
)

// A pinned agent posts each open dialog of its session into its DM once.
// The owner's ✅ answers option 1, and the resolution edits the post. A
// typed "3" answers the next dialog and never reaches the session.
func TestDialogTickPostsReactsEdits(t *testing.T) {
	const dm = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	root := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Name: "alpha", Label: "Claude Code · proj @ mac"}); err != nil {
		t.Fatal(err)
	}
	withBuzzSelf(t)
	lr := agentRelayForTest(t)
	ownerKey := nostr.Generate()
	owner := ownerKey.Public().Hex()
	savedOwner, savedRead, savedEdit := buzzOwner, readReactions, editMessage
	t.Cleanup(func() { buzzOwner, readReactions, editMessage = savedOwner, savedRead, savedEdit })
	buzzOwner = owner
	var posts, edits []string
	savedPost := postAnswer
	t.Cleanup(func() { postAnswer = savedPost })
	postAnswer = func(channel, content string, _ time.Duration) (string, error) {
		posts = append(posts, channel+": "+content)
		return fmt.Sprintf("%064x", len(posts)), nil
	}
	readReactions = func(string, time.Duration) ([]reaction, error) {
		return []reaction{{Emoji: "✅", PubKeys: []string{owner}}}, nil
	}
	editMessage = func(event, content string, _ time.Duration) error {
		edits = append(edits, event+": "+content)
		return nil
	}
	s := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: canonicalTempDir(t)}, "test")
	s.ready, s.lastChannel = true, dm

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
	s.dialogTick(now) // posts
	s.dialogTick(now) // reads the ✅
	want := dm + ": Claude Code · proj @ mac wants to run Bash:\n$ ls /tmp\n\n1. Allow once\n2. Allow this session\n3. Deny\n\nReact ✅ = 1, ❌ = Deny, or reply a number / yes / no."
	if len(posts) != 1 || posts[0] != want {
		t.Fatalf("posts = %q; want the dialog once", posts)
	}
	if a, ok, _ := dialog.ReadAnswer(dir, first.ID); !ok || a.Option != 1 {
		t.Fatalf("answer = %+v, %v; want option 1", a, ok)
	}
	if _, err := dialog.Resolve(dir, dialog.Resolved{ID: first.ID, Outcome: dialog.OutcomeAllowed, Label: "Allow once", By: dialog.ByBuzz, At: now}); err != nil {
		t.Fatal(err)
	}
	s.dialogTick(now)
	s.dialogTick(now)
	if len(edits) != 1 || edits[0] != fmt.Sprintf("%064x", 1)+": "+strings.TrimPrefix(want, dm+": ")+"\n\n→ Allow once (Buzz)" {
		t.Fatalf("edits = %q; want the post edited once with the outcome", edits)
	}

	second := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Edit", Text: "main.go", Options: options, OpenedAt: time.Now()}
	if err := dialog.Open(dir, second); err != nil {
		t.Fatal(err)
	}
	readReactions = func(string, time.Duration) ([]reaction, error) { return nil, nil }
	s.dialogTick(now)
	answer := signedBy(t, ownerKey, "3", dm)
	lr.Inject(answer)
	params, _ := json.Marshal(promptParams{SessionID: "s", Prompt: []contentBlock{{Type: "text", Text: buzzPrompt(dm, owner, "3")}}, Meta: eventMeta(answer)})
	run, _, rpcErr := s.beginPrompt(params)
	if rpcErr != nil || run == nil {
		t.Fatalf("beginPrompt = %v", rpcErr)
	}
	if _, rpcErr := run(func(any) error { return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if a, ok, _ := dialog.ReadAnswer(dir, second.ID); !ok || a.Option != 3 {
		t.Fatalf("answer = %+v, %v; want option 3", a, ok)
	}
	if got := inboxPrompts(t, root); len(got) != 0 {
		t.Fatalf("the answer reached the session: %v", got)
	}
	if last := posts[len(posts)-1]; last != dm+": Answered." {
		t.Fatalf("posts = %q; want the answer acknowledged", posts)
	}
}

// signedBy is a plaintext message with content in channel, signed by key.
func signedBy(t *testing.T, key nostr.SecretKey, content, channel string, tags ...nostr.Tag) nostr.Event {
	t.Helper()
	evt := nostr.Event{CreatedAt: nostr.Now(), Kind: 9, Content: content, Tags: append(nostr.Tags{{"h", channel}}, tags...)}
	if err := evt.Sign(key); err != nil {
		t.Fatal(err)
	}
	return evt
}

// eventMeta is the ACP _meta with which buzz-acp names a prompt's event.
func eventMeta(evt nostr.Event) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"nostr": map[string]string{"eventId": evt.ID.Hex()}})
	return raw
}

// buzzPrompt is a buzz-acp DM prompt in dm whose event from sender (a hex
// pubkey, or "" for no From line) carries content.
func buzzPrompt(dm, sender, content string) string {
	from := ""
	if sender != "" {
		from = "From: Someone (npub: npub1x, hex: " + sender + ")\n"
	}
	return "<context>\nScope: dm\nChannel: DM (#" + dm + ")\n</context>\n<buzz-event type=\"@mention\">\nEvent ID: " + strings.Repeat("a", 64) +
		"\n" + from + "Content: " + content + "\nTags: [[\"h\",\"" + dm + "\"]]\n</buzz-event>"
}

// Regression (review of the dialog poster): a ✅ reaction became the literal
// answer of a free-text dialog, a reply with no known sender answered it, and
// a two-line answer to a two-question dialog reached it as one line. Only the
// owner's typed reply answers a free-text dialog, with its line breaks.
func TestFreeTextDialogTakesOnlyTheOwnersTypedAnswer(t *testing.T) {
	const dm = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	withBuzzSelf(t)
	lr := agentRelayForTest(t)
	ownerKey := nostr.Generate()
	owner := ownerKey.Public().Hex()
	savedOwner, savedRead, savedPost := buzzOwner, readReactions, postAnswer
	t.Cleanup(func() { buzzOwner, readReactions, postAnswer = savedOwner, savedRead, savedPost })
	buzzOwner = owner
	postAnswer = func(string, string, time.Duration) (string, error) { return strings.Repeat("f", 64), nil }
	readReactions = func(string, time.Duration) ([]reaction, error) {
		return []reaction{{Emoji: "✅", PubKeys: []string{owner}}, {Emoji: "❌", PubKeys: []string{owner}}}, nil
	}
	s := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: canonicalTempDir(t)}, "test")
	s.lastChannel = dm
	dir, err := dialog.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	req := dialog.Request{ID: dialog.NewID(), Harness: "codex", Text: "Which database?\nMigrate now?", FreeText: true, OpenedAt: time.Now()}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	s.dialogTick(time.Now()) // posts
	s.dialogTick(time.Now()) // reads the reactions
	if a, ok, _ := dialog.ReadAnswer(dir, req.ID); ok {
		t.Fatalf("a reaction answered a free-text dialog: %+v", a)
	}
	other := signedBy(t, nostr.Generate(), "1", dm)
	lr.Inject(other)
	if _, a, ok := s.dialogReply(dm, other.ID.Hex(), buzzPrompt(dm, owner, "1")); ok {
		t.Fatalf("another signer answered: %+v", a)
	}
	answer := signedBy(t, ownerKey, "postgres\nyes", dm)
	lr.Inject(answer)
	_, a, ok := s.dialogReply(dm, answer.ID.Hex(), buzzPrompt(dm, owner, "postgres\nyes"))
	if !ok || a.Text != "postgres\nyes" {
		t.Fatalf("owner answer = %+v, %v; want the two lines", a, ok)
	}
}

// Review of the owner checks: a dialog took its answer and its sender from
// the prompt text buzz-acp wrote. Only the signed event decides: with no
// verified event the dialog stays pending, and its signed content wins over
// the text.
func TestDialogAnswerIsTheVerifiedSignedEvent(t *testing.T) {
	const dm = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	cases := []struct {
		name     string
		body     string // what the prompt text says the owner wrote
		signed   string // the signed content; "" for no event on the relay
		wantOK   bool
		wantDeny bool
	}{
		{name: "no verified event", body: "1"},
		{name: "signed deny, text allow", body: "1", signed: "no", wantOK: true, wantDeny: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
			if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha"}); err != nil {
				t.Fatal(err)
			}
			withBuzzSelf(t)
			lr := agentRelayForTest(t)
			ownerKey := nostr.Generate()
			owner := ownerKey.Public().Hex()
			savedOwner, savedRead, savedPost := buzzOwner, readReactions, postAnswer
			t.Cleanup(func() { buzzOwner, readReactions, postAnswer = savedOwner, savedRead, savedPost })
			buzzOwner = owner
			postAnswer = func(string, string, time.Duration) (string, error) { return strings.Repeat("f", 64), nil }
			readReactions = func(string, time.Duration) ([]reaction, error) { return nil, nil }
			s := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: canonicalTempDir(t)}, "test")
			s.lastChannel = dm
			dir, err := dialog.DirFor("alpha")
			if err != nil {
				t.Fatal(err)
			}
			req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", OpenedAt: time.Now(),
				Options: []dialog.Option{{N: 1, Label: "Allow once"}, {N: 2, Label: "Deny", Deny: true}}}
			if err := dialog.Open(dir, req); err != nil {
				t.Fatal(err)
			}
			s.dialogTick(time.Now()) // posts

			id := strings.Repeat("d", 64)
			if tc.signed != "" {
				evt := signedBy(t, ownerKey, tc.signed, dm)
				lr.Inject(evt)
				id = evt.ID.Hex()
			}
			_, a, ok := s.dialogReply(dm, id, buzzPrompt(dm, owner, tc.body))
			if ok != tc.wantOK || (ok && (a.Option == 2) != tc.wantDeny) {
				t.Fatalf("dialogReply = %+v, %v; want ok=%v deny=%v", a, ok, tc.wantOK, tc.wantDeny)
			}
		})
	}
}

// Review of the verified owner gate: a signed owner "yes" from before a
// dialog opened, or one that had already answered another dialog, answered
// the new dialog. A signature says who wrote it, not what it answers.
func TestOwnerAnswerMustBeNewerThanItsDialogAndUnused(t *testing.T) {
	s, dir, owner, lr := ownerDialogServer(t)
	old := nostr.Event{CreatedAt: nostr.Timestamp(time.Now().Add(-time.Hour).Unix()), Kind: 9, Content: "yes", Tags: nostr.Tags{{"h", dmChannel}}}
	if err := old.Sign(owner); err != nil {
		t.Fatal(err)
	}
	lr.Inject(old)
	first := openPostedDialog(t, s, dir, "1")
	params, _ := json.Marshal(promptParams{SessionID: "s", Meta: eventMeta(old),
		Prompt: []contentBlock{{Type: "text", Text: buzzPrompt(dmChannel, buzzOwner, "yes")}}})
	if run, _, rpcErr := s.beginPrompt(params); rpcErr == nil && run != nil {
		_, _ = run(func(any) error { return nil })
	}
	if a, ok, _ := dialog.ReadAnswer(dir, first.ID); ok {
		t.Fatalf("an owner event signed an hour before the dialog opened answered it: %+v", a)
	}

	used := signedBy(t, owner, "yes", dmChannel)
	lr.Inject(used)
	_, a, ok := s.dialogReply(dmChannel, used.ID.Hex(), buzzPrompt(dmChannel, buzzOwner, "yes"))
	if !ok {
		t.Fatal("a fresh owner yes did not answer the only open dialog")
	}
	if _, err := dialog.WriteAnswer(dir, a); err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.Resolve(dir, dialog.Resolved{ID: first.ID, Outcome: dialog.OutcomeAllowed, By: dialog.ByBuzz, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	openPostedDialog(t, s, dir, "2")
	if _, a, ok := s.dialogReply(dmChannel, used.ID.Hex(), buzzPrompt(dmChannel, buzzOwner, "yes")); ok {
		t.Fatalf("the event that answered one dialog answered the next: %+v", a)
	}
}

// Review of the verified owner gate: an e tag that only mentions a dialog's
// post answered that dialog, and a reply naming two open dialogs answered the
// older one. Only a NIP-10 root or reply names the target, and it must name
// exactly one open dialog.
func TestOwnerAnswerTargetsTheOneDialogItRepliesTo(t *testing.T) {
	first, second, other := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	cases := []struct {
		name string
		tags []nostr.Tag
		want int // 0: no answer; n: the dialog posted as the nth
	}{
		{name: "mention", tags: []nostr.Tag{{"e", other, "", "root"}, {"e", other, "", "reply"}, {"e", first, "", "mention"}}},
		{name: "both dialogs", tags: []nostr.Tag{{"e", first, "", "root"}, {"e", second, "", "reply"}}},
		{name: "reply to the second", tags: []nostr.Tag{{"e", second, "", "root"}, {"e", second, "", "reply"}}, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, dir, owner, lr := ownerDialogServer(t)
			reqs := []dialog.Request{openPostedDialog(t, s, dir, "1"), openPostedDialog(t, s, dir, "2")}
			evt := signedBy(t, owner, "yes", dmChannel, tc.tags...)
			lr.Inject(evt)
			_, a, ok := s.dialogReply(dmChannel, evt.ID.Hex(), buzzPrompt(dmChannel, buzzOwner, "yes"))
			if tc.want == 0 && ok || tc.want != 0 && (!ok || a.ID != reqs[tc.want-1].ID) {
				t.Fatalf("dialogReply = %+v, %v; want dialog %d", a, ok, tc.want)
			}
		})
	}
}

// Review of the verified owner gate: an hour-old signed "mirror on" turned
// the mirror on again after the owner had switched it off.
func TestOldMirrorOnDoesNotUndoALaterOff(t *testing.T) {
	s, _ := machineServer(t)
	b := liveSession(t, "alpha")
	b.Harness = "claude"
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, s, b, dmChannel)
	lr := agentRelayForTest(t)
	owner := nostr.Generate()
	buzzOwner = owner.Public().Hex()
	old := nostr.Event{CreatedAt: nostr.Timestamp(time.Now().Add(-time.Hour).Unix()), Kind: 9, Content: "mirror on", Tags: nostr.Tags{{"h", dmChannel}}}
	if err := old.Sign(owner); err != nil {
		t.Fatal(err)
	}
	lr.Inject(old)
	if err := mirror.SetOff("alpha", true); err != nil {
		t.Fatal(err)
	}
	if name, on, at, ok := s.mirrorSwitch(dmChannel, old.ID.Hex(), buzzPrompt(dmChannel, buzzOwner, "mirror on")); ok {
		_, _ = s.answerMirrorSwitch("s", dmChannel, name, on, at, func(any) error { return nil })
	}
	if dir, err := mirror.DirFor("alpha"); err != nil || !mirror.Off(dir) {
		t.Fatalf("an hour-old mirror on undid the later mirror off (err %v)", err)
	}
}

// ownerDialogServer is a pinned agent of binding alpha whose owner is owner,
// with a local relay that serves the events a test injects.
func ownerDialogServer(t *testing.T) (*Server, string, nostr.SecretKey, *relaytest.Relay) {
	t.Helper()
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	withBuzzSelf(t)
	lr := agentRelayForTest(t)
	owner := nostr.Generate()
	savedOwner, savedPost := buzzOwner, postAnswer
	t.Cleanup(func() { buzzOwner, postAnswer = savedOwner, savedPost })
	buzzOwner = owner.Public().Hex()
	postAnswer = func(string, string, time.Duration) (string, error) { return strings.Repeat("f", 64), nil }
	s := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: canonicalTempDir(t)}, "test")
	s.ready = true
	s.sessions["s"] = &sessionState{}
	dir, err := dialog.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, owner, lr
}

// openPostedDialog opens a two-option dialog now and records its post in
// dmChannel as the event whose id repeats digit.
func openPostedDialog(t *testing.T, s *Server, dir, digit string) dialog.Request {
	t.Helper()
	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", OpenedAt: time.Now(),
		Options: []dialog.Option{{N: 1, Label: "Allow once"}, {N: 2, Label: "Deny", Deny: true}}}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	if err := s.writeDialogPost(filepath.Join(s.dialogPostDir("alpha"), req.ID+".json"), dialogPost{Channel: dmChannel, Event: strings.Repeat(digit, 64)}); err != nil {
		t.Fatal(err)
	}
	return req
}

// Regression (review of the dialog poster): the dialogs of a gone binding
// were expired and removed in one tick, so the seam still waiting never read
// its resolution. The directory stays while the seam runs, for seamGrace.
func TestGoneBindingKeepsDialogsUntilTheSeamLeaves(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	s := NewServer(Config{RemoteBinding: true, Pin: "gone", StateDir: canonicalTempDir(t)}, "test")
	dir, err := dialog.DirFor("gone")
	if err != nil {
		t.Fatal(err)
	}
	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", OpenedAt: time.Now(), PID: os.Getpid()}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.dropGoneDialogs(now)
	if res, ok, _ := dialog.ReadResolved(dir, req.ID); !ok || res.Outcome != dialog.OutcomeExpired {
		t.Fatalf("resolution = %+v, %v; want expired", res, ok)
	}
	s.dropGoneDialogs(now)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the dialogs went while the seam runs: %v", err)
	}
	s.dropGoneDialogs(now.Add(seamGrace + time.Second))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the dialogs stayed after seamGrace: %v", err)
	}
}

// Regression (agent-message-queue-udg review): dropGoneDialogs judged each
// name from the snapshot it took before a loop of relay calls. Session bbb
// attached again during aaa's edit, and its open dialog was expired as the
// dialog of an ended session.
func TestDropGoneDialogsKeepsTheDialogOfASessionThatAttachedAgain(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	s := NewServer(Config{RemoteBinding: true, Machine: true, StateDir: canonicalTempDir(t)}, "test")
	liveRoot := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(liveRoot, "agent"); err != nil {
		t.Fatal(err)
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	// bbb: old binding, ended (root gone).
	old := binding.Binding{Carrier: binding.CarrierMailbox, Root: filepath.Join(liveRoot, "gone"), Handle: "agent", Holder: holder, Name: "bbb", Label: "old"}
	if err := binding.WriteNamed(old); err != nil {
		t.Fatal(err)
	}
	// aaa: no binding, one resolved dialog with a post, so dialogClose edits it.
	aDir, _ := dialog.DirFor("aaa")
	aReq := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", OpenedAt: time.Now()}
	if err := dialog.Open(aDir, aReq); err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.Resolve(aDir, dialog.Resolved{ID: aReq.ID, Outcome: dialog.OutcomeExpired, By: dialog.ByTimeout, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(dialogPost{Channel: "c", Event: "ev", Text: "t"})
	if _, err := fsq.WriteFileAtomicFollowLinks(s.dialogPostDir("aaa"), aReq.ID+".json", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// bbb: open dialog.
	bDir, _ := dialog.DirFor("bbb")
	bReq := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", OpenedAt: time.Now(), PID: os.Getpid()}
	if err := dialog.Open(bDir, bReq); err != nil {
		t.Fatal(err)
	}
	saved := editMessage
	t.Cleanup(func() { editMessage = saved })
	editMessage = func(string, string, time.Duration) error {
		// During aaa's relay call, bbb re-attaches: a live binding.
		nb := old
		nb.Root = liveRoot
		if err := binding.WriteNamed(nb); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	s.dropGoneDialogs(time.Now())
	if res, ok, _ := dialog.ReadResolved(bDir, bReq.ID); ok {
		t.Fatalf("the open dialog of live session bbb was resolved: %+v", res)
	}
}
