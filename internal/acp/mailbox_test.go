package acp

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/mailprompt"
)

// mailboxSender is the AMQ handle the replies of these tests go to.
const mailboxSender = "buzz"

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

// The reply poll skips listing an inbox whose mtime has not moved; a reply
// that lands after the inbox sat idle still ends the turn.
func TestReplyAfterAnIdleInboxEndsTheTurn(t *testing.T) {
	root := canonicalTempDir(t)
	for _, h := range []string{mailboxSender, "agent"} {
		if err := fsq.EnsureAgentDirs(root, h); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Root: root, Me: mailboxSender, To: "agent"}
	d, err := DeliverCockpitPrompt(cfg, "build the thing", cockpitThread("s"), strings.Repeat("8", 64))
	if err != nil {
		t.Fatal(err)
	}
	inbox := fsq.AgentInboxNew(root, mailboxSender)
	idle := time.Now().Add(-time.Hour)
	if err := os.Chtimes(inbox, idle, idle); err != nil {
		t.Fatal(err)
	}
	s, scan := NewServer(cfg, "test"), mailprompt.NewScan(inbox)
	for range 2 {
		if _, found, err := s.replyForDelivery(d, scan); err != nil || found {
			t.Fatalf("idle inbox: found=%v err=%v", found, err)
		}
	}
	replyAs(t, root, d.Thread, d.MessageID, format.KindAnswer, "done")
	if got, found, err := s.replyForDelivery(d, scan); err != nil || !found || got != "done" {
		t.Fatalf("reply after an idle inbox: %q found=%v err=%v", got, found, err)
	}
}
