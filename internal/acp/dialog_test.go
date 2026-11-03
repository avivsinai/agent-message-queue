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
	want := dm + ": Claude Code · proj @ mac wants to run Bash:\n$ ls /tmp\n\n1. Allow once\n2. Allow this session\n3. Deny\nReact ✅ = 1, ❌ = Deny, or reply a number / yes / no."
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

// A dialog of a session never connected to Buzz makes the machine agent
// create its approvals channel once, add the owner, and post the dialog
// there under the session's own label.
func TestUnboundDialogReachesTheApprovalsChannel(t *testing.T) {
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
	dir, err := dialog.DirFor(dialog.MachineBinding)
	if err != nil {
		t.Fatal(err)
	}
	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", Text: "$ make", Label: "claude · proj",
		Options: []dialog.Option{{N: 1, Label: "Allow once"}, {N: 2, Label: "Deny", Deny: true}}, OpenedAt: time.Now(), PID: os.Getpid()}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())
	tick(s, time.Now())
	if r := channelState(t, s, dialog.MachineBinding); r.State != channelActive || r.ChannelID != machineChanB || !strings.HasPrefix(r.Label, "Approvals") {
		t.Fatalf("approvals record = %+v", r)
	}
	if calls.count("channels create") != 1 || calls.count("channels add-member --channel "+machineChanB+" --pubkey "+buzzOwner) != 1 {
		t.Fatalf("calls = %q; want one create and the owner added", calls.list())
	}
	if len(posts) != 1 || !strings.HasPrefix(posts[0], machineChanB+": claude · proj wants to run Bash:\n$ make") {
		t.Fatalf("posts = %q; want the dialog in the approvals channel once", posts)
	}
}
