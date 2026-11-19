package machineagent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

func sessionQuestion(t *testing.T) (*standaloneFixture, string, format.Header) {
	t.Helper()
	f := standalone(t)
	b := liveSession(t, "alpha")
	activeChannel(t, f.s, b, machineChanA)
	f.groupState(t, machineChanA)
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	h := format.Header{Schema: format.CurrentSchema, ID: id, From: b.Handle, To: []string{"user"},
		Thread: "p2p/agent__user", Kind: format.KindQuestion, Subject: "When do we ship?",
		Created: now.UTC().Format(time.RFC3339Nano), ReplyBy: now.Add(time.Hour).Format(time.RFC3339)}
	data, err := (format.Message{Header: h, Body: "Tomorrow?"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(b.Root, "user"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(b.Root, "user"), id+".md"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return f, b.Root, h
}

// A connected mailbox posts an owner question once, with its deadline and reply instructions.
func TestAskIsPostedOnce(t *testing.T) {
	f, _, h := sessionQuestion(t)
	sends := 0
	f.postsOffline(&atomic.Bool{}, func() { sends++ })
	f.s.askTick(time.Now())
	f.s.askTick(time.Now())
	posts := f.agentMessages(machineChanA)
	if sends != 1 || len(posts) != 1 {
		t.Fatalf("got %d sends and %d posts, want one of each", sends, len(posts))
	}
	deadline, _ := time.Parse(time.RFC3339, h.ReplyBy)
	want := h.Subject + "\n\nTomorrow?\n\nDefault at " + deadline.Local().Format("Mon 2 Jan 15:04 MST") + ".\nReply in this thread to answer."
	if !strings.Contains(posts[0].Content, want) || writeTag(posts[0], "p") != f.s.id.Owner {
		t.Fatalf("question = %q, tags %v", posts[0].Content, posts[0].Tags)
	}
}

// An owner reply in the post's thread closes the original question in AMQ and in Buzz.
func TestOwnerThreadReplyAnswersAsk(t *testing.T) {
	f, root, h := sessionQuestion(t)
	f.s.askTick(time.Now())
	posts := f.agentMessages(machineChanA)
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want one question", len(posts))
	}
	quit := make(chan struct{})
	f.s.owners.start(quit)
	t.Cleanup(func() { close(quit); f.s.owners.stop() })
	evt := f.ownerSignedAt(t, machineChanA, "mirror off", time.Now(), nostr.Tag{"e", posts[0].ID.Hex(), "", "reply"})
	waitClosed(t, f.s.Owner(buzzop.Inbound{Channel: machineChanA, Event: evt}))
	ids := inboxPrompts(t, root)
	if len(ids) != 1 {
		t.Fatalf("inbox = %v, want one answer", ids)
	}
	m, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, h.From), ids[0]+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.From != "user" || m.Header.Kind != format.KindAnswer || m.Header.Thread != h.Thread ||
		!slices.Equal(m.Header.Refs, []string{h.ID}) || !slices.Contains(m.Header.Labels, "buzz") ||
		!slices.Contains(m.Header.Labels, "nostr:"+evt.ID.Hex()) || m.Body != evt.Content+"\n" {
		t.Fatalf("answer = %+v", m)
	}
	if got := f.lastAgentPost(machineChanA); got != "Answered." {
		t.Fatalf("acknowledgement = %q", got)
	}
	f.s.askTick(time.Now())
	if edits := f.agentEdits(machineChanA, posts[0].ID); len(edits) != 1 || !strings.HasSuffix(edits[0].Content, "\n\n→ Answered (Buzz)") {
		t.Fatalf("edited question = %+v", edits)
	}
}
