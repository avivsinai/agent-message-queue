package machineagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

const (
	machineChanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	machineChanB = "0b2c39a1-6f1e-4d0a-9b3a-5c1d2e3f4a5b"
)

// machineCalls reads the machine agent's channel writes from the relay of
// its fixture f.
type machineCalls struct {
	f *standaloneFixture
	// from is the number of publishes the relay had at the last reset.
	from int
}

// list is each channel write of the agent that reached the relay since the
// last reset, a resend again, also one the relay refused or dropped, in the
// words of the buzz command that makes the same change.
func (c *machineCalls) list() []string {
	var out []string
	for _, evt := range c.f.relay.Attempts()[c.from:] {
		if evt.PubKey.Hex() != c.f.s.id.Agent {
			continue
		}
		h := writeTag(evt, "h")
		switch evt.Kind {
		case 9007:
			out = append(out, "channels create --name "+writeTag(evt, "name")+" --type "+writeTag(evt, "channel_type")+
				" --visibility "+writeTag(evt, "visibility")+" --description "+writeTag(evt, "about"))
		case 9000:
			out = append(out, "channels add-member --channel "+h+" --pubkey "+writeTag(evt, "p")+" --role "+writeTag(evt, "role"))
		case 9:
			call := "messages send --channel " + h + " --content " + evt.Content
			if p := writeTag(evt, "p"); p != "" {
				call += " --mention " + p
			}
			out = append(out, call)
		case 9002:
			verb := "unarchive"
			if writeTag(evt, "archived") == "true" {
				verb = "archive"
			}
			out = append(out, "channels "+verb+" --channel "+h)
		}
	}
	return out
}

// creates are the distinct create events of the agent that reached the
// relay, refused or dropped ones too, in the order first received.
func (c *machineCalls) creates() []nostr.Event {
	var out []nostr.Event
	for _, evt := range c.f.relay.Attempts() {
		if evt.Kind == 9007 && evt.PubKey.Hex() == c.f.s.id.Agent &&
			!slices.ContainsFunc(out, func(e nostr.Event) bool { return e.ID == evt.ID }) {
			out = append(out, evt)
		}
	}
	return out
}

func (c *machineCalls) reset() { c.from = len(c.f.relay.Attempts()) }

func (c *machineCalls) count(prefix string) int {
	n := 0
	for _, call := range c.list() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// machineServer is a standalone machine agent on the in-process relay,
// which stores the group state each accepted channel write changes
// (relayState). opts set the relay up before its first connection.
func machineServer(t *testing.T, opts ...func(*relaytest.Relay)) (*Agent, *machineCalls) {
	t.Helper()
	var self atomic.Pointer[standaloneFixture]
	f := standalone(t, append(opts, func(r *relaytest.Relay) {
		r.Accepted = func(evt nostr.Event) {
			if f := self.Load(); f != nil {
				f.relayState(t, evt)
			}
		}
	})...)
	self.Store(f)
	return f.s, &machineCalls{f: f}
}

// newMachineServer is a second machine worker on the transport and state
// directory of f: the worker after a restart.
func newMachineServer(t *testing.T, f *standaloneFixture) *Agent {
	t.Helper()
	s := New(Config{Identity: f.s.id, Transport: f.s.direct})
	t.Cleanup(s.resign)
	return s
}

func tick(s *Agent, now time.Time) { s.machineTick(context.Background(), now) }

// liveSession binds a live mailbox session (held by this test process)
// under name and returns its binding.
func liveSession(t *testing.T, name string) binding.Binding {
	t.Helper()
	root := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Holder: holder,
		Label: "claude · proj/" + name + " @ box", Name: name}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// activeChannel writes an active record of channel for b.
func activeChannel(t *testing.T, s *Agent, b binding.Binding, channel string) {
	t.Helper()
	r := channelRecord{Relay: s.id.RelayURL, Machine: s.id.Agent, ChannelID: channel, Name: b.Name,
		Identity: b.SessionIdentity(), Label: sessionAgentName(b), Reg: "reg-" + b.Name, State: channelActive,
		OwnerAdded: true, Welcomed: true, Creates: 1}
	if err := s.save(&r, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func channelState(t *testing.T, s *Agent, name string) channelRecord {
	t.Helper()
	r, ok := readChannelRecord(s.slotPath(name))
	if !ok {
		t.Fatalf("no record for %s", name)
	}
	return r
}

func onlyRetired(t *testing.T, s *Agent) channelRecord {
	t.Helper()
	retired := s.retiredRecords()
	if len(retired) != 1 {
		t.Fatalf("retired records = %+v", retired)
	}
	return retired[0]
}

// The workers keep one read of the bindings and records while their files
// are unchanged; a binding written after that read is served on the next
// tick. The test waits out fsq.FileVersionSettle so the first read is kept.
func TestMachineServesABindingWrittenAfterTheSnapshot(t *testing.T) {
	s, _ := machineServer(t)
	root := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Holder: holder,
		Label: "claude · proj/alpha @ box", Name: "alpha"}
	activeChannel(t, s, b, machineChanA)
	_, dir, err := binding.WatchPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(fsq.FileVersionSettle + 100*time.Millisecond)
	if got := s.dialogTargets(); len(got) != 0 {
		t.Fatalf("targets before the binding = %+v, want none", got)
	}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	got := s.dialogTargets()
	if len(got) != 1 || got[0].name != "alpha" || got[0].channel != machineChanA {
		t.Fatalf("targets after the binding = %+v, want alpha in its channel", got)
	}
}

// The machine leader makes a private channel for a live session, adds the
// owner as an admin, welcomes them with a mention, and activates it.
//
// Regressions (PR #1279 review): a write the relay refused still carries an
// event id, and was taken for success; only accepted:true is. A record that
// exists but cannot be read was taken for first use, and a new channel
// replaced it.
func TestMachineProvisionsAChannelForALiveSession(t *testing.T) {
	s, rec := machineServer(t)
	b := liveSession(t, "alpha")
	liveSession(t, "beta")
	corrupt := []byte("{not json")
	if err := os.MkdirAll(s.channelDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.slotPath("beta"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	rec.f.relay.RejectKind.Store(9000) // the first add-member is refused
	now := time.Now()
	tick(s, now)
	if r := channelState(t, s, "alpha"); r.OwnerAdded || r.Welcomed || r.State != channelCreating {
		t.Fatalf("after a refused add-member: %+v", r)
	}
	rec.f.relay.RejectKind.Store(0)
	tick(s, now.Add(time.Minute))

	r := channelState(t, s, "alpha")
	addMember := "channels add-member --channel " + r.Chosen + " --pubkey " + s.id.Owner + " --role admin"
	want := []string{
		"channels create --name " + b.Label + " --type stream --visibility private --description amq-remote " + r.Reg,
		addMember,
		addMember,
		"messages send --channel " + r.Chosen + " --content AMQ Remote: this channel reaches " + b.Label + ". Write here to talk to that session. --mention " + s.id.Owner,
	}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("buzz calls = %q\nwant %q", got, want)
	}
	if r.State != channelActive || r.ChannelID != r.Chosen || r.Creates != 1 || !r.OwnerAdded || !r.Welcomed {
		t.Fatalf("record = %+v", r)
	}
	if raw, _ := os.ReadFile(s.slotPath("beta")); string(raw) != string(corrupt) {
		t.Fatalf("the unreadable record was replaced: %q", raw)
	}
}

// A prompt from a session's active channel reaches that session only.
func TestMachinePromptInAnActiveChannelReachesItsSession(t *testing.T) {
	s, rec := machineServer(t)
	alpha, beta := liveSession(t, "alpha"), liveSession(t, "beta")
	activeChannel(t, s, alpha, machineChanA)
	activeChannel(t, s, beta, machineChanB)
	rec.f.startMachine(t)

	rec.f.s.Owner(rec.f.ownerSays(t, machineChanA, "hello"))
	for deadline := time.Now().Add(5 * time.Second); len(inboxPrompts(t, alpha.Root)) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if got := inboxPrompts(t, alpha.Root); len(got) != 1 {
		t.Fatalf("alpha got %d prompts, want 1", len(got))
	}
	if got := inboxPrompts(t, beta.Root); len(got) != 0 {
		t.Fatalf("beta got %d prompts, want 0", len(got))
	}
}

// A prompt from a channel no active record names reaches no session; the
// answer lists the live sessions and their channels. Owner drops a message
// in such a channel, so the test runs the turn itself, with no claim and no
// active record of its channel.
func TestMachinePromptInAnUnknownChannelGetsTheDirectory(t *testing.T) {
	s, rec := machineServer(t)
	alpha := liveSession(t, "alpha")
	activeChannel(t, s, alpha, machineChanA)

	s.runOwnerTurn(machineChanB, rec.f.ownerSays(t, machineChanB, "hello").Event, make(chan struct{}))
	said := rec.f.lastAgentPost(machineChanB)
	if got := inboxPrompts(t, alpha.Root); len(got) != 0 {
		t.Fatalf("alpha got %d prompts from an unknown channel", len(got))
	}
	if !strings.Contains(said, alpha.Label) || !strings.Contains(said, "Run /amq-remote in a session to connect it.") {
		t.Fatalf("directory = %q", said)
	}
}

// Detach retires the record at once, says the session ended in its channel
// and archives the channel; the same session attaching again gets that
// channel back, unarchived.
//
// Regressions (PR #1279 review): a refused archive (event id kept,
// accepted:false) was recorded as archived; and a restore after an archive
// that was sent but not confirmed activated the channel without an
// unarchive, and a refused unarchive still activated it.
func TestMachineDetachArchivesAndReattachRestoresTheChannel(t *testing.T) {
	s, rec := machineServer(t)
	alpha := liveSession(t, "alpha")
	activeChannel(t, s, alpha, machineChanA)
	rec.f.groupState(t, machineChanA)
	rec.f.relay.RejectKind.Store(9002) // archives and unarchives are refused
	if _, err := binding.Retire(binding.RemovedDetached, func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tick(s, now)
	if r := onlyRetired(t, s); r.State != channelRetired || r.Archived || r.RetiredFrom != channelActive {
		t.Fatalf("retired record after a refused archive = %+v", r)
	}
	if _, ok := readChannelRecord(s.slotPath("alpha")); ok {
		t.Fatal("the retired record still holds the name's slot")
	}
	// On the direct transport a restore reads the relay's state: only a
	// channel the relay shows archived needs an unarchive. The archive is
	// retried after channelRetryGap and is accepted.
	rec.f.relay.RejectKind.Store(0)
	tick(s, now.Add(channelRetryGap+time.Minute))
	if r := onlyRetired(t, s); !r.Archived {
		t.Fatalf("retired record after an accepted archive = %+v", r)
	}

	rec.f.relay.RejectKind.Store(9002)
	if err := binding.WriteNamed(alpha); err != nil { // the same session again
		t.Fatal(err)
	}
	now = now.Add(channelRetryGap + 2*time.Minute)
	tick(s, now)
	if _, ok := readChannelRecord(s.slotPath("alpha")); ok {
		t.Fatal("a refused unarchive activated the channel")
	}
	rec.f.relay.RejectKind.Store(0)
	tick(s, now.Add(time.Minute))

	want := []string{
		"messages send --channel " + machineChanA + " --content Disconnected: " + alpha.Label + ". This channel no longer reaches it until the session connects again.",
		"channels archive --channel " + machineChanA,
		"channels archive --channel " + machineChanA,
		"channels unarchive --channel " + machineChanA,
		"channels unarchive --channel " + machineChanA,
		"messages send --channel " + machineChanA + " --content Connected again: " + alpha.Label + ".",
	}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("buzz calls = %q\nwant %q", got, want)
	}
	if r := channelState(t, s, "alpha"); r.State != channelActive || r.ChannelID != machineChanA || r.Archived {
		t.Fatalf("restored record = %+v", r)
	}

	// Regression (PR #1279 review): the restore unlinked the retired copy,
	// and if that unlink's sync failed, the next retirement saw the copy
	// absent and wrote the retired slot without syncing, so a stop could
	// bring the stale copy back as the newer one. The absence must be
	// synced before the slot says retired.
	var synced []string
	defer fsq.SetPackageSyncDirFaultForTest(func(dir string) error {
		if strings.HasPrefix(dir, s.channelDir()) {
			synced = append(synced, dir)
		}
		return nil
	})()
	if _, err := binding.Retire(binding.RemovedDetached, func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
		t.Fatal(err)
	}
	tick(s, now.Add(3*time.Minute))
	if len(synced) == 0 || synced[0] != s.retiredDir() {
		t.Fatalf("directory syncs on retirement = %q; want the retired store synced first", synced)
	}
}

// Regression (agent-message-queue-eqzq): a link share and a mailbox binding
// of one running session both held active owner channels.
func TestMachineGivesOneChannelToASessionWithTwoBindings(t *testing.T) {
	s, rec := machineServer(t)
	root, holder := canonicalTempDir(t), selfHolder(t)
	const session = "s1"
	native := binding.Binding{Carrier: binding.CarrierNative, Root: root, Target: "pi:agent", Harness: "pi",
		NativeSession: session, Holder: holder, Name: "native", Label: "pi · native @ box"}
	if err := binding.WriteNamed(native); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, s, native, machineChanA)
	rec.f.groupState(t, machineChanA)
	now := time.Now()
	tick(s, now)

	mailbox := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Harness: "pi",
		HarnessSession: session, Holder: holder, Name: "mailbox", Label: "pi · mailbox @ box"}
	if err := binding.WriteNamed(mailbox); err != nil {
		t.Fatal(err)
	}
	tick(s, now.Add(time.Minute))
	mailboxRecord := channelState(t, s, mailbox.Name)
	if mailboxRecord.State != channelActive {
		t.Fatalf("mailbox record = %+v; want active", mailboxRecord)
	}
	if r, exists, err := loadChannelRecord(s.slotPath(native.Name)); exists || err != nil {
		t.Fatalf("mailbox channel is %q and native slot is %q (error %v); want no native slot", mailboxRecord.State, r.State, err)
	}
	r := onlyRetired(t, s)
	if r.RetiredBy != "joined" || !r.Archived || r.ChannelID != machineChanA {
		t.Fatalf("retired native record = %+v; want the joined, archived channel", r)
	}
	raw, err := os.ReadFile(s.recordPath(r))
	if err != nil {
		t.Fatal(err)
	}
	var joined struct {
		JoinedTo string `json:"joined_to"`
	}
	if err := json.Unmarshal(raw, &joined); err != nil || joined.JoinedTo != mailbox.Label {
		t.Fatalf("retired record joined_to = %q, %v; want %q", joined.JoinedTo, err, mailbox.Label)
	}
	events := rec.f.relay.Events()
	if !slices.ContainsFunc(events, func(evt nostr.Event) bool {
		return evt.Kind == 39000 && evt.PubKey == rec.f.relayKey.Public() &&
			writeTag(evt, "d") == machineChanA && writeTag(evt, "archived") == "true"
	}) {
		t.Fatal("the relay did not record the archive of the native channel")
	}
	message := "Joined: " + native.Label + " now uses the channel " + strconv.Quote(mailbox.Label) + ". This channel no longer reaches it."
	if !slices.ContainsFunc(events, func(evt nostr.Event) bool {
		return evt.Kind == 9 && writeTag(evt, "h") == machineChanA && evt.Content == message
	}) {
		t.Fatalf("the native channel did not receive %q", message)
	}

	if _, err := binding.Retire(binding.RemovedDetached, func(b binding.Binding) bool { return b.Name == mailbox.Name }); err != nil {
		t.Fatal(err)
	}
	tick(s, now.Add(2*time.Minute))
	if r := channelState(t, s, native.Name); r.State != channelActive || r.ChannelID != machineChanA || r.Archived {
		t.Fatalf("restored native record = %+v; want the original active channel", r)
	}
}

// appServerSession is a bound Codex app-server thread, with no holder, and
// what the daemon at its socket answers: its pid and the loaded threads,
// with the number of loaded-thread probes.
type appServerSession struct {
	b      binding.Binding
	loaded []string
	err    error // the loaded-thread probe's failure, if any
	pid    int
	probes int
}

// bindAppServerThread binds a loaded thread whose binding records daemon.
func bindAppServerThread(t *testing.T, daemon launch.HolderIdentity) *appServerSession {
	t.Helper()
	const thread = "0198a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	root := canonicalTempDir(t)
	a := &appServerSession{loaded: []string{thread}, pid: os.Getpid(),
		b: binding.Binding{Root: root, Target: "codex:" + thread, NativeSession: thread, Harness: "codex", Name: "codex-proj",
			Label: "codex · proj @ box", Identity: binding.NativeIdentity("codex", root, root),
			AppServer: binding.AppServerThread{Socket: "/daemon.sock", Thread: thread, Daemon: daemon}}}
	saved := newAppServerView
	t.Cleanup(func() { newAppServerView = saved })
	newAppServerView = func() *codex.View {
		return codex.NewView(func(string) ([]string, error) { a.probes++; return a.loaded, a.err },
			func(string) (int, error) { return a.pid, nil })
	}
	if err := binding.WriteNamed(a.b); err != nil {
		t.Fatal(err)
	}
	return a
}

func selfHolder(t *testing.T) launch.HolderIdentity {
	t.Helper()
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	return self
}

// A Codex app-server session records no holder, only its thread, so the
// holder alone left it unknown: the machine agent made no channel for it and
// never archived one. Its liveness comes from the daemon that runs the
// thread.
func TestMachineServesACodexAppServerThreadUntilItEnds(t *testing.T) {
	s, rec := machineServer(t)
	a := bindAppServerThread(t, selfHolder(t))
	now := time.Now()
	tick(s, now)
	r := channelState(t, s, a.b.Name)
	if r.State != channelActive || r.ChannelID != r.Chosen {
		t.Fatalf("record for a loaded thread = %+v", r)
	}
	channel := r.ChannelID
	if a.probes != 1 {
		t.Fatalf("daemon asked %d times in one tick, want 1", a.probes)
	}

	a.loaded = nil // the thread ends in the same daemon
	rec.reset()
	tick(s, now.Add(time.Minute))
	if r := onlyRetired(t, s); r.RetiredBy != retiredEnded || !r.Archived {
		t.Fatalf("record after the thread ended = %+v", r)
	}
	want := []string{
		"messages send --channel " + channel + " --content Session ended: " + a.b.Label + ". This channel no longer reaches it.",
		"channels archive --channel " + channel,
	}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("buzz calls = %q\nwant %q", got, want)
	}
}

// Review of the app-server channel: when the daemon a thread was bound in
// ended, nothing proved the session ended, so its channel was never
// archived. A Codex update restarts the daemon, so a gone daemon archives
// only on two ticks in a row, and the thread loaded again gets the channel
// back.
func TestMachineArchivesACodexThreadWhoseDaemonIsGone(t *testing.T) {
	s, rec := machineServer(t)
	dead := selfHolder(t)
	dead.ProcessStart = laterStart(t, dead.ProcessStart)
	a := bindAppServerThread(t, dead) // a restarted daemon (this process) has it loaded
	now := time.Now()
	tick(s, now)
	r := channelState(t, s, a.b.Name)
	if r.State != channelActive {
		t.Fatalf("record for a thread loaded in a restarted daemon = %+v", r)
	}
	channel := r.ChannelID

	a.loaded = nil
	rec.reset()
	tick(s, now.Add(time.Minute))
	if r := channelState(t, s, a.b.Name); r.State != channelActive || len(rec.list()) != 0 {
		t.Fatalf("after one tick with the daemon gone: record %+v, buzz calls %q", r, rec.list())
	}
	tick(s, now.Add(2*time.Minute))
	if r := onlyRetired(t, s); r.RetiredBy != retiredEnded || !r.Archived {
		t.Fatalf("record after two ticks with the daemon gone = %+v", r)
	}

	a.loaded = []string{a.b.AppServer.Thread}
	tick(s, now.Add(3*time.Minute))
	want := []string{
		"messages send --channel " + channel + " --content Session ended: " + a.b.Label + ". This channel no longer reaches it.",
		"channels archive --channel " + channel,
		"channels unarchive --channel " + channel,
		"messages send --channel " + channel + " --content Connected again: " + a.b.Label + ".",
	}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("buzz calls = %q\nwant %q", got, want)
	}
	if r := channelState(t, s, a.b.Name); r.State != channelActive || r.ChannelID != channel || r.Archived {
		t.Fatalf("restored record = %+v", r)
	}
}

// Review of the gone-daemon rule: a probe that timed out counted as the
// thread being gone, so a session live in a replacement daemon was archived
// after two slow ticks.
func TestMachineKeepsACodexThreadWhoseProbeTimesOut(t *testing.T) {
	s, rec := machineServer(t)
	dead := selfHolder(t)
	dead.ProcessStart = laterStart(t, dead.ProcessStart)
	a := bindAppServerThread(t, dead) // a replacement daemon has it loaded
	now := time.Now()
	tick(s, now)
	if r := channelState(t, s, a.b.Name); r.State != channelActive {
		t.Fatalf("record for a thread loaded in a replacement daemon = %+v", r)
	}

	a.err = fmt.Errorf("thread/loaded/list: %w", context.DeadlineExceeded)
	rec.reset()
	tick(s, now.Add(time.Minute))
	tick(s, now.Add(2*time.Minute))
	if r := channelState(t, s, a.b.Name); r.State != channelActive || len(rec.list()) != 0 {
		t.Fatalf("after two timed-out probes: record %+v, buzz calls %q", r, rec.list())
	}
}

// Review of the detach wording: a Codex app-server session records no
// holder, so only the prune that removed its binding proved it ended. Its
// channel must still say "Session ended", from the reason in the tombstone.
func TestMachineSaysSessionEndedForAPrunedSession(t *testing.T) {
	s, rec := machineServer(t)
	alpha := liveSession(t, "alpha")
	activeChannel(t, s, alpha, machineChanA)
	rec.f.groupState(t, machineChanA)
	if _, err := binding.Retire(binding.RemovedEnded, func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())
	want := "messages send --channel " + machineChanA + " --content Session ended: " + alpha.Label + ". This channel no longer reaches it."
	if got := rec.list(); len(got) == 0 || got[0] != want {
		t.Fatalf("buzz calls = %q; want first %q", got, want)
	}
}

// Regression (PR #1279 review): a slot whose removal was not yet durable
// can come back after a stop, holding the record as it was when it was
// retired. Its recovery saved that stale copy over the retired record and
// lost the confirmed archive, so the same session came back without an
// unarchive. This starts from the two records such a stop can leave; it is
// not a proof of live crash behavior.
func TestMachineRecoveredSlotKeepsTheRetiredOutcome(t *testing.T) {
	s, rec := machineServer(t)
	alpha := liveSession(t, "alpha")
	stale := channelRecord{Relay: s.id.RelayURL, Machine: s.id.Agent, ChannelID: machineChanA, Name: "alpha",
		Identity: alpha.SessionIdentity(), Label: sessionAgentName(alpha), Reg: "reg-alpha", State: channelRetired,
		RetiredBy: retiredEnded, RetiredFrom: channelActive, OwnerAdded: true, Welcomed: true, Creates: 1}
	raw, _ := json.Marshal(stale)
	if err := os.MkdirAll(s.channelDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.slotPath("alpha"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	progressed := stale
	progressed.SaidEnded, progressed.ArchiveSent, progressed.Archived = true, true, true
	if err := s.save(&progressed, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The relay shows the archive that the retired record confirmed.
	rec.f.groupState(t, machineChanA)
	rec.f.relayState(t, nostr.Event{Kind: 9002, Tags: nostr.Tags{{"h", machineChanA}, {"archived", "true"}}})
	if _, err := binding.Retire(binding.RemovedDetached, func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tick(s, now)
	if r := onlyRetired(t, s); !r.Archived || !r.ArchiveSent || len(rec.list()) != 0 {
		t.Fatalf("recovery lost the confirmed archive: %+v, calls %q", r, rec.list())
	}
	if _, found, _ := loadChannelRecord(s.slotPath("alpha")); found {
		t.Fatal("the stale slot was not removed")
	}

	if err := binding.WriteNamed(alpha); err != nil { // the same session again
		t.Fatal(err)
	}
	tick(s, now.Add(time.Minute))
	want := []string{
		"channels unarchive --channel " + machineChanA,
		"messages send --channel " + machineChanA + " --content Connected again: " + alpha.Label + ".",
	}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("buzz calls = %q\nwant %q", got, want)
	}
}

// Regression (field, 2026-11-14): a machine stop left retired records whose
// archive was recorded as sent but never reached the relay. When the same
// sessions attached again, the relay refused each unarchive with "channel is
// not archived", the restore kept that as a failure, and the sessions had no
// channel ("its channel is being made") until a channel reset.
//
// On the direct transport the restore reads the relay's state first: a
// channel it shows not archived is restored with no unarchive sent.
func TestMachineRestoresAChannelTheRelaySaysIsNotArchived(t *testing.T) {
	s, rec := machineServer(t)
	alpha := liveSession(t, "alpha")
	r := channelRecord{Relay: s.id.RelayURL, Machine: s.id.Agent, ChannelID: machineChanA, Name: "alpha",
		Identity: alpha.SessionIdentity(), Label: sessionAgentName(alpha), Reg: "reg-alpha", State: channelRetired,
		RetiredBy: retiredEnded, RetiredFrom: channelActive, OwnerAdded: true, Welcomed: true, Creates: 1,
		SaidEnded: true, ArchiveSent: true}
	if err := s.save(&r, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec.f.groupState(t, machineChanA)
	tick(s, time.Now())
	want := []string{
		"messages send --channel " + machineChanA + " --content Connected again: " + alpha.Label + ".",
	}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("buzz calls = %q\nwant %q", got, want)
	}
	if got := channelState(t, s, "alpha"); got.State != channelActive || got.ChannelID != machineChanA || got.ArchiveSent {
		t.Fatalf("restored record = %+v", got)
	}
}

// Regression (contract review): the buzz CLI makes a fresh channel id on
// each create, so a create whose outcome is unknown must never be sent
// again. The leader looks for the channel instead and adopts the one it
// owns whose description holds the record's reg.
//
// On the direct transport the agent chooses the channel id: the recovery of
// an uncertain create sends the same stored event again (the same event
// id), or adopts the chosen channel once the relay's state shows it. One
// registration never gets a second signed create.
func TestMachineUncertainCreateAdoptsOneChannelAndNeverCreatesAgain(t *testing.T) {
	s, rec := machineServer(t, func(r *relaytest.Relay) { r.DropKind.Store(9007) })
	liveSession(t, "alpha")
	now := time.Now()
	tick(s, now)
	if r := channelState(t, s, "alpha"); r.State != channelUncertain {
		t.Fatalf("after an unknown outcome: %+v", r)
	}
	rec.f.reconnectAll() // the relay closed the connection of the dropped create
	tick(s, now.Add(time.Minute))
	tick(s, now.Add(11*time.Minute))

	r := channelState(t, s, "alpha")
	creates := rec.creates()
	if len(creates) != 1 || writeTag(creates[0], "h") != r.Chosen || writeTag(creates[0], "about") != "amq-remote "+r.Reg {
		t.Fatalf("distinct creates = %+v, want one of the chosen channel %s; calls %q", creates, r.Chosen, rec.list())
	}
	if r.State != channelActive || r.ChannelID != r.Chosen {
		t.Fatalf("record = %+v", r)
	}
	tick(s, now.Add(30*time.Minute))
	if n := len(rec.creates()); n != 1 {
		t.Fatalf("distinct creates after a later tick = %d, want 1", n)
	}
}

// Regression (contract review): a create that returns after the session
// detached must not activate a retired route: its channel is recorded under
// the retired reg and archived, never welcomed.
//
// Regression (PR #1279 review): a create whose worker stopped before the
// result was written, followed by a detach and a restart, was retired as a
// record that made no channel, so the channel it made was never looked for.
// On the direct transport it is looked for by its chosen id in the relay's
// state.
func TestMachineCreateThatReturnsAfterDetachIsArchived(t *testing.T) {
	t.Run("create returns after detach", func(t *testing.T) {
		var detached atomic.Bool
		s, rec := machineServer(t, func(r *relaytest.Relay) {
			r.BeforePublish = func(evt nostr.Event) {
				if evt.Kind == 9007 && detached.CompareAndSwap(false, true) {
					if _, err := binding.Retire(binding.RemovedDetached, func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
						t.Error(err)
					}
				}
			}
		})
		liveSession(t, "alpha")
		tick(s, time.Now())

		if n := rec.count("channels add-member") + rec.count("messages send"); n != 0 {
			t.Fatalf("a retired channel was set up: %q", rec.list())
		}
		r := onlyRetired(t, s)
		if rec.count("channels archive --channel "+r.Chosen) != 1 {
			t.Fatalf("calls = %q, want the late channel archived", rec.list())
		}
		if r.ChannelID != r.Chosen || !r.Archived || r.Welcomed {
			t.Fatalf("retired record = %+v", r)
		}
	})
	t.Run("create result lost, then detach and restart", func(t *testing.T) {
		var slot atomic.Pointer[string]
		var atCreate atomic.Pointer[[]byte]
		s, rec := machineServer(t, func(r *relaytest.Relay) {
			r.BeforePublish = func(evt nostr.Event) {
				// What the disk holds while the create runs is what a worker
				// that stops here leaves behind.
				if path := slot.Load(); evt.Kind == 9007 && path != nil {
					raw, _ := os.ReadFile(*path)
					atCreate.Store(&raw)
				}
			}
		})
		path := s.slotPath("alpha")
		slot.Store(&path)
		liveSession(t, "alpha")
		tick(s, time.Now())
		s.resign()
		if err := os.WriteFile(path, *atCreate.Load(), 0o600); err != nil {
			t.Fatal(err)
		}
		rec.reset()
		if _, err := binding.Retire(binding.RemovedDetached, func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
			t.Fatal(err)
		}
		tick(newMachineServer(t, rec.f), time.Now())

		r := onlyRetired(t, s)
		if rec.count("channels archive --channel "+r.Chosen) != 1 {
			t.Fatalf("calls = %q, want the lost create looked for and its channel archived", rec.list())
		}
		if r.ChannelID != r.Chosen || !r.Archived {
			t.Fatalf("retired record = %+v", r)
		}
	})
}

// Regression (PR #1279 review): the machine worker's stop did not reach the
// lifecycle: a tick went on creating channels for every binding after the
// worker was told to stop. A create cut off by the stop stays uncertain, and
// nothing else starts.
//
// The stop comes while the relay holds the first create, and that create
// gets no OK, so its outcome is unknown.
func TestMachineStopEndsTheBatchAndLeavesTheCutCreateUncertain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, rec := machineServer(t, func(r *relaytest.Relay) {
		r.DropKind.Store(9007)
		r.BeforePublish = func(evt nostr.Event) {
			if evt.Kind == 9007 {
				cancel()
			}
		}
	})
	liveSession(t, "alpha")
	liveSession(t, "beta")
	s.machineTick(ctx, time.Now())

	creates := rec.creates()
	if got := rec.list(); len(got) != 1 || len(creates) != 1 {
		t.Fatalf("calls after the stop = %q, want only the cut create", got)
	}
	reg := strings.TrimPrefix(writeTag(creates[0], "about"), "amq-remote ")
	cut := 0
	for _, name := range []string{"alpha", "beta"} {
		r, found, _ := loadChannelRecord(s.slotPath(name))
		switch {
		case found && r.Reg == reg:
			cut++
			if r.State != channelUncertain || !r.Doubted || r.Creates != 1 {
				t.Fatalf("cut create = %+v", r)
			}
		case found && (r.Creates != 0 || r.Pending):
			t.Fatalf("a new create started after the stop: %+v", r)
		}
	}
	if cut != 1 {
		t.Fatalf("%d records hold the cut create's registration, want 1", cut)
	}
}

// laterStart is a process start token 100 s after tok, in the platform's
// own format (sec.nsec, or clock ticks).
func laterStart(t *testing.T, tok string) string {
	t.Helper()
	sec, nsec, dotted := strings.Cut(tok, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		t.Skip("unknown process start format")
	}
	if dotted {
		return strconv.FormatInt(n+100, 10) + "." + nsec
	}
	return strconv.FormatInt(n+100*100, 10)
}

// Regression (PR #1279 review): a newer worker that led and then left kept
// its marker, and the older worker that took the lead could never replace
// it, so the machine agent read offline while it ran. The lead holder
// replaces the marker of a departed newer worker.
func TestMachineLeaderReplacesTheMarkerOfADepartedNewerWorker(t *testing.T) {
	s, _ := machineServer(t)
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	saved := workerIdentity
	t.Cleanup(func() { workerIdentity = saved })
	workerIdentity = self
	departed := self
	departed.PID, departed.ProcessStart = 1<<30, laterStart(t, self.ProcessStart)
	dir := s.agentMarkerDir()
	raw, _ := json.Marshal(agentMarker{Name: machineName, PubKey: s.id.Agent, Worker: departed, Updated: time.Now().UTC().Format(time.RFC3339)})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, s.id.Agent+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())

	if m, _ := readAgentMarker(filepath.Join(dir, s.id.Agent+".json")); m.Worker != self {
		t.Fatalf("marker worker = %+v, want the leader %+v", m.Worker, self)
	}
}

// Regression (contract review): a binding name reused by another session
// identity must never reach the old session's channel.
func TestMachineReusedNameWithAnotherIdentityNeverRoutesToTheOldChannel(t *testing.T) {
	s, rec := machineServer(t)
	old := liveSession(t, "alpha")
	activeChannel(t, s, old, machineChanA)
	replacement := liveSession(t, "alpha") // same name, another root

	// The owner's message runs as its route's turn, as Owner queues it, with
	// no machine tick that could retire the old record first.
	s.runOwnerTurn(machineChanA, rec.f.ownerSays(t, machineChanA, "hello").Event, make(chan struct{}))
	said := rec.f.lastAgentPost(machineChanA)
	if got := inboxPrompts(t, replacement.Root); len(got) != 0 {
		t.Fatalf("the replacement session got %d prompts from the old channel", len(got))
	}
	if !strings.Contains(said, "not connected") {
		t.Fatalf("reply = %q", said)
	}
}
