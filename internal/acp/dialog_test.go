package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
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
	owner := strings.Repeat("0b", 32)
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
	prompt := "<context>\nScope: dm\nChannel: DM (#" + dm + ")\n</context>\n<buzz-event type=\"@mention\">\nEvent ID: " + strings.Repeat("a", 64) +
		"\nFrom: Owner (npub: npub1x, hex: " + owner + ")\nContent: 3\nTags: [[\"h\",\"" + dm + "\"]]\n</buzz-event>"
	params, _ := json.Marshal(promptParams{SessionID: "s", Prompt: []contentBlock{{Type: "text", Text: prompt}}})
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

// A dialog of a session never connected to Buzz writes the session's own
// dialogs binding; the machine agent makes that session's channel, posts the
// dialog there under the session's label, and answers a chat prompt in it
// with approvals only.
func TestDialogsBindingGetsTheSessionsOwnChannel(t *testing.T) {
	s, calls := machineServer(t)
	calls.reply = func(args []string) ([]byte, error) {
		if strings.Join(args[:2], " ") == "channels create" {
			return []byte(`{"accepted":true,"channel_id":"` + machineChanB + `"}`), nil
		}
		return []byte(`{"accepted":true,"event_id":"ev"}`), nil
	}
	var posts []string
	savedPost := postAnswer
	t.Cleanup(func() { postAnswer = savedPost })
	postAnswer = func(channel, content string, _ time.Duration) (string, error) {
		posts = append(posts, channel+": "+content)
		return strings.Repeat("f", 64), nil
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	name, err := dialog.BindingFor(dialog.Session{Harness: "claude", Native: "s-1", Cwd: "/work/proj", Holder: holder})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := dialog.DirFor(name)
	if err != nil {
		t.Fatal(err)
	}
	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", Text: "$ make",
		Options: []dialog.Option{{N: 1, Label: "Allow once"}, {N: 2, Label: "Deny", Deny: true}}, OpenedAt: time.Now(), PID: os.Getpid()}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())
	s.dialogTick(time.Now())
	if r := channelState(t, s, name); r.State != channelActive || r.ChannelID != machineChanB {
		t.Fatalf("record of %s = %+v", name, r)
	}
	label := binding.SessionLabel("claude", "proj", binding.Machine())
	if len(posts) != 1 || !strings.HasPrefix(posts[0], machineChanB+": "+label+" wants to run Bash:\n$ make") {
		t.Fatalf("posts = %q; want the dialog in the session's channel once", posts)
	}
	if said := channelPrompt(t, s, machineChanB); said != "This session shows approvals only. Run /amq-remote in it to chat from Buzz." {
		t.Fatalf("a chat prompt said %q", said)
	}
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
	owner := strings.Repeat("0b", 32)
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
	for _, sender := range []string{"", strings.Repeat("0c", 32)} {
		if _, a, ok := s.dialogReply(dm, buzzPrompt(dm, sender, "1")); ok {
			t.Fatalf("sender %q answered: %+v", sender, a)
		}
	}
	_, a, ok := s.dialogReply(dm, buzzPrompt(dm, owner, "postgres\nyes"))
	if !ok || a.Text != "postgres\nyes" {
		t.Fatalf("owner answer = %+v, %v; want the two lines", a, ok)
	}
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
