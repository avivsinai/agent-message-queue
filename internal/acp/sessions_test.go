package acp

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// sessionChannelServer is a machine agent whose creates make machineChanB,
// with a session record of this test process (holder) named name.
func sessionChannelServer(t *testing.T, holder launch.HolderIdentity) (*Server, *machineCalls, string, dialog.SessionRecord) {
	t.Helper()
	s, calls := machineServer(t)
	calls.reply = func(args []string) ([]byte, error) {
		if strings.Join(args[:2], " ") == "channels create" {
			return []byte(`{"accepted":true,"channel_id":"` + machineChanB + `"}`), nil
		}
		return []byte(`{"accepted":true,"event_id":"ev"}`), nil
	}
	rec := dialog.SessionRecord{Harness: "claude", Session: "s-1", Label: "claude · proj @ box", Holder: holder}
	name, err := dialog.RecordSession(rec, "claude-proj")
	if err != nil {
		t.Fatal(err)
	}
	return s, calls, name, rec
}

func selfHolder(t *testing.T) launch.HolderIdentity {
	t.Helper()
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	return holder
}

// A session record of a running session gets a channel of its own, named by
// its label; its dialog is posted there, and a chat prompt in it answers
// with approvals only.
func TestSessionRecordGetsItsOwnChannel(t *testing.T) {
	s, _, name, rec := sessionChannelServer(t, selfHolder(t))
	var posts []string
	savedPost := postAnswer
	t.Cleanup(func() { postAnswer = savedPost })
	postAnswer = func(channel, content string, _ time.Duration) (string, error) {
		posts = append(posts, channel+": "+content)
		return strings.Repeat("f", 64), nil
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
	if r := channelState(t, s, name); r.State != channelActive || r.ChannelID != machineChanB || r.Label != rec.Label {
		t.Fatalf("record of %s = %+v", name, r)
	}
	if len(posts) != 1 || !strings.HasPrefix(posts[0], machineChanB+": "+rec.Label+" wants to run Bash:\n$ make") {
		t.Fatalf("posts = %q; want the dialog in the session's channel once", posts)
	}
	if said := channelPrompt(t, s, machineChanB); said != "This session shows approvals only. Run /amq-remote in it to chat from Buzz." {
		t.Fatalf("a chat prompt said %q", said)
	}
}

// When the session's process attaches, its binding takes the session's
// channel over: the record moves to the binding's name with the same
// channel, and the session record is dropped.
func TestSessionChannelMovesToTheBindingOfItsProcess(t *testing.T) {
	holder := selfHolder(t)
	s, calls, name, _ := sessionChannelServer(t, holder)
	tick(s, time.Now())
	if r := channelState(t, s, name); r.State != channelActive {
		t.Fatalf("session channel = %+v", r)
	}
	root := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Holder: holder, Label: "claude · proj/s @ box", Name: "alpha"}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())
	if r := channelState(t, s, "alpha"); r.State != channelActive || r.ChannelID != machineChanB || r.Identity != b.SessionIdentity() {
		t.Fatalf("binding's record = %+v; want the session's channel", r)
	}
	if _, found, _ := loadChannelRecord(s.slotPath(name)); found {
		t.Fatal("the session's record is still there")
	}
	if _, ok, _ := dialog.ReadSession(name); ok {
		t.Fatal("the session record is still there")
	}
	if n := calls.count("channels create"); n != 1 {
		t.Fatalf("creates = %d; want the one channel", n)
	}
}

// A session record whose process is gone has its channel retired and the
// record dropped.
func TestSessionChannelRetiresWhenItsProcessIsGone(t *testing.T) {
	gone := selfHolder(t)
	if gone.BootID == "" {
		t.Skip("no boot id on this platform, so a start mismatch proves nothing")
	}
	gone.ProcessStart += "0"
	s, _, name, rec := sessionChannelServer(t, gone)
	r := channelRecord{Relay: normalizeRelay(buzzRelayURL), Machine: buzzSelf, ChannelID: machineChanB, Name: name,
		Identity: sessionIdentity(rec), Label: rec.Label, Reg: "reg-s", State: channelActive, OwnerAdded: true, Welcomed: true, Creates: 1}
	if err := s.save(&r, time.Now()); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())
	if got := onlyRetired(t, s); got.Reg != "reg-s" || got.RetiredBy != retiredEnded {
		t.Fatalf("retired = %+v; want the session's channel, ended", got)
	}
	if _, ok, _ := dialog.ReadSession(name); ok {
		t.Fatal("the session record is still there")
	}
}
