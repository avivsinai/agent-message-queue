package machineagent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// mailboxRoute is a standalone machine agent whose route machineChanA
// serves the live mailbox session "alpha", with the turn timings of the
// mailbox tests. Change a timing before the workers start.
func mailboxRoute(t *testing.T, opts ...func(*relaytest.Relay)) (*standaloneFixture, binding.Binding) {
	t.Helper()
	f := standalone(t, opts...)
	b := liveSession(t, "alpha")
	activeChannel(t, f.s, b, machineChanA)
	f.s.cfg.TurnTimeout, f.s.cfg.PollInterval, f.s.cfg.HeartbeatInterval = 2*time.Second, 5*time.Millisecond, 20*time.Millisecond
	return f, b
}

// machine starts the fixture's workers. stop ends them once and waits for
// them; cleanup stops them when the test did not. After stop the workers
// may start again.
func (f *standaloneFixture) machine(t *testing.T) (stop func()) {
	t.Helper()
	end, ok := f.s.Start()
	if !ok {
		t.Fatal("the machine agent did not start")
	}
	var once sync.Once
	stop = func() { once.Do(end) }
	t.Cleanup(stop)
	return stop
}

// promptOf waits up to 5 s for the one prompt in b's inbox and returns its
// id and message.
func promptOf(t *testing.T, b binding.Binding) (string, format.Message) {
	t.Helper()
	var ids []string
	for deadline := time.Now().Add(5 * time.Second); len(ids) == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		ids = inboxPrompts(t, b.Root)
	}
	if len(ids) != 1 {
		t.Fatalf("inbox prompts = %v; want one", ids)
	}
	msg, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(b.Root, b.Handle), ids[0]+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return ids[0], msg
}

// replyInCur delivers an answer to prompt that is already in the buzz
// inbox's cur, the way another consumer that drained it leaves it, so a
// running turn never sees it in new. It returns the reply's id.
func replyInCur(t *testing.T, root, thread, prompt, body string) string {
	t.Helper()
	staging := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(staging, mailboxSender); err != nil {
		t.Fatal(err)
	}
	id := replyAs(t, staging, thread, prompt, format.KindAnswer, body)
	if err := os.Rename(filepath.Join(fsq.AgentInboxNew(staging, mailboxSender), id+".md"), filepath.Join(fsq.AgentInboxCur(root, mailboxSender), id+".md")); err != nil {
		t.Fatal(err)
	}
	return id
}

// waitAgentPosts waits up to 5 s until the agent has n messages in channel.
func (f *standaloneFixture) waitAgentPosts(t *testing.T, channel string, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(f.agentMessages(channel)) < n && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	if got := len(f.agentMessages(channel)); got < n {
		t.Fatalf("%d agent messages in %s; want %d", got, channel, n)
	}
}

// agentTexts are the contents of the agent's messages in channel in stored
// order.
func (f *standaloneFixture) agentTexts(channel string) []string {
	var out []string
	for _, evt := range f.agentMessages(channel) {
		out = append(out, evt.Content)
	}
	return out
}

// offlinePosts is the direct transport with posts that are proved not sent
// while down is set, as buzzdirect reports a send with no relay
// connection. sending, when set, runs before each send that goes out.
type offlinePosts struct {
	Transport
	down    *atomic.Bool
	sending func()
}

func (p offlinePosts) Send(ctx context.Context, m buzzop.Message) buzzop.Result {
	if p.down.Load() {
		return buzzop.Result{Outcome: buzzop.NotSent, Reason: buzzop.ReasonNoConnection}
	}
	if p.sending != nil {
		p.sending()
	}
	return p.Transport.Send(ctx, m)
}

// postsOffline routes the fixture's posts through offlinePosts. Call it
// before the workers start.
func (f *standaloneFixture) postsOffline(down *atomic.Bool, sending func()) {
	f.s.posts = boundedPosts{s: f.s, t: offlinePosts{Transport: f.s.direct, down: down, sending: sending}}
}
