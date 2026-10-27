package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

const (
	machineChanA = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	machineChanB = "0b2c39a1-6f1e-4d0a-9b3a-5c1d2e3f4a5b"
)

// machineCalls records the machine agent's buzz calls and answers each
// from reply, keyed by the first two arguments.
type machineCalls struct {
	mu    sync.Mutex
	calls []string
	reply func(args []string) ([]byte, error)
}

func (c *machineCalls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *machineCalls) count(prefix string) int {
	n := 0
	for _, call := range c.list() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// machineServer is a machine agent with a Buzz key, an owner and a relay.
// Each buzz call goes to the recorder; nothing reaches a relay.
func machineServer(t *testing.T) (*Server, *machineCalls) {
	t.Helper()
	if !lock.AdvisoryLockAvailable() {
		t.Skip("no advisory lock on this platform")
	}
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	savedSelf, savedOwner, savedKey, savedURL, savedCall := buzzSelf, buzzOwner, buzzRelayKey, buzzRelayURL, buzzCall
	t.Cleanup(func() {
		buzzSelf, buzzOwner, buzzRelayKey, buzzRelayURL, buzzCall = savedSelf, savedOwner, savedKey, savedURL, savedCall
	})
	buzzSelf, buzzOwner = strings.Repeat("e", 64), strings.Repeat("0", 64)
	buzzRelayURL = "wss://relay.example/"
	buzzRelayKey = relayKey(buzzRelayURL)
	rec := &machineCalls{reply: func([]string) ([]byte, error) { return []byte(`{"accepted":true,"event_id":"ev"}`), nil }}
	buzzCall = func(_ time.Duration, args ...string) ([]byte, error) {
		rec.mu.Lock()
		rec.calls = append(rec.calls, strings.Join(args, " "))
		reply := rec.reply
		rec.mu.Unlock()
		return reply(args)
	}
	s := NewServer(Config{RemoteBinding: true, Machine: true, StateDir: canonicalTempDir(t),
		TurnTimeout: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond}, "test")
	t.Cleanup(s.resign)
	return s, rec
}

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
func activeChannel(t *testing.T, s *Server, b binding.Binding, channel string) {
	t.Helper()
	r := channelRecord{Relay: normalizeRelay(buzzRelayURL), Machine: buzzSelf, ChannelID: channel, Name: b.Name,
		Identity: b.SessionIdentity(), Label: sessionAgentName(b), Reg: "reg-" + b.Name, State: channelActive,
		OwnerAdded: true, Welcomed: true, Creates: 1}
	if err := s.save(&r, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func channelState(t *testing.T, s *Server, name string) channelRecord {
	t.Helper()
	r, ok := readChannelRecord(filepath.Join(s.channelDir(), name+".json"))
	if !ok {
		t.Fatalf("no record for %s", name)
	}
	return r
}

func onlyRetired(t *testing.T, s *Server) channelRecord {
	t.Helper()
	retired := s.retiredRecords()
	if len(retired) != 1 {
		t.Fatalf("retired records = %+v", retired)
	}
	return retired[0]
}

// channelPrompt sends one prompt whose host context names channel and
// returns the text the agent said.
func channelPrompt(t *testing.T, s *Server, channel string) string {
	t.Helper()
	// Without an owner the served worker's machine tick provisions
	// nothing, so routing sees exactly the records the test wrote.
	buzzOwner = ""
	live := startServer(t, s.cfg)
	live.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2}}`)
	live.read()
	live.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/"}}`)
	sid := live.read()["result"].(map[string]any)["sessionId"].(string)
	blocks, _ := json.Marshal([]contentBlock{
		{Type: "text", Text: "<context>\nScope: channel\nSession scope: channel\nChannel: some channel (#" + channel + ")\n</context>"},
		{Type: "text", Text: "<buzz-event>\nhello\n</buzz-event>"},
	})
	live.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":%s}}`, sid, blocks))
	said := ""
	for {
		value := live.read()
		if _, ok := value["result"]; ok {
			return said
		}
		if value["error"] != nil {
			t.Fatalf("prompt: %v", value)
		}
		if update, ok := value["params"].(map[string]any)["update"].(map[string]any); ok && update["sessionUpdate"] == "agent_message_chunk" {
			said += update["content"].(map[string]any)["text"].(string)
		}
	}
}

// The machine leader makes a private channel for a live session, adds the
// owner as an admin, welcomes them with a mention, and activates it.
func TestMachineProvisionsAChannelForALiveSession(t *testing.T) {
	s, rec := machineServer(t)
	b := liveSession(t, "alpha")
	rec.reply = func(args []string) ([]byte, error) {
		if args[1] == "create" {
			return []byte(`{"accepted":true,"channel_id":"` + machineChanA + `"}`), nil
		}
		return []byte(`{"accepted":true,"event_id":"ev"}`), nil
	}
	s.machineTick(time.Now())

	r := channelState(t, s, "alpha")
	want := []string{
		"channels create --name " + b.Label + " --type stream --visibility private --description amq-remote " + r.Reg,
		"channels add-member --channel " + machineChanA + " --pubkey " + buzzOwner + " --role admin",
		"messages send --channel " + machineChanA + " --content AMQ Remote: this channel reaches " + b.Label + ". Write here to talk to that session. --mention " + buzzOwner,
	}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("buzz calls = %q\nwant %q", got, want)
	}
	if r.State != channelActive || r.ChannelID != machineChanA || r.Creates != 1 || !r.OwnerAdded || !r.Welcomed {
		t.Fatalf("record = %+v", r)
	}
}

// A prompt from a session's active channel reaches that session only.
func TestMachinePromptInAnActiveChannelReachesItsSession(t *testing.T) {
	s, _ := machineServer(t)
	alpha, beta := liveSession(t, "alpha"), liveSession(t, "beta")
	activeChannel(t, s, alpha, machineChanA)
	activeChannel(t, s, beta, machineChanB)

	channelPrompt(t, s, machineChanA)
	if got := inboxPrompts(t, alpha.Root); len(got) != 1 {
		t.Fatalf("alpha got %d prompts, want 1", len(got))
	}
	if got := inboxPrompts(t, beta.Root); len(got) != 0 {
		t.Fatalf("beta got %d prompts, want 0", len(got))
	}
}

// A prompt from a channel no active record names reaches no session; the
// answer lists the live sessions and their channels.
func TestMachinePromptInAnUnknownChannelGetsTheDirectory(t *testing.T) {
	s, _ := machineServer(t)
	alpha := liveSession(t, "alpha")
	activeChannel(t, s, alpha, machineChanA)

	said := channelPrompt(t, s, machineChanB)
	if got := inboxPrompts(t, alpha.Root); len(got) != 0 {
		t.Fatalf("alpha got %d prompts from an unknown channel", len(got))
	}
	if !strings.Contains(said, alpha.Label) || !strings.Contains(said, "Run /amq-remote in a session to connect it.") {
		t.Fatalf("directory = %q", said)
	}
}

// Detach retires the record at once, says the session ended in its channel
// and archives the channel.
func TestMachineDetachRetiresAndArchivesTheChannel(t *testing.T) {
	s, rec := machineServer(t)
	alpha := liveSession(t, "alpha")
	activeChannel(t, s, alpha, machineChanA)
	if _, err := binding.Retire(func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
		t.Fatal(err)
	}
	s.machineTick(time.Now())

	want := []string{
		"messages send --channel " + machineChanA + " --content Session ended: " + alpha.Label + ". This channel no longer reaches it.",
		"channels archive --channel " + machineChanA,
	}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Fatalf("buzz calls = %q\nwant %q", got, want)
	}
	if r := onlyRetired(t, s); r.State != channelRetired || !r.Archived || r.RetiredFrom != channelActive {
		t.Fatalf("retired record = %+v", r)
	}
	if _, ok := readChannelRecord(filepath.Join(s.channelDir(), "alpha.json")); ok {
		t.Fatal("the retired record still holds the name's slot")
	}
}

// Regression (contract review): the buzz CLI makes a fresh channel id on
// each create, so a create whose outcome is unknown must never be sent
// again. The leader looks for the channel instead and adopts the one it
// owns whose description holds the record's reg.
func TestMachineUncertainCreateAdoptsOneChannelAndNeverCreatesAgain(t *testing.T) {
	s, rec := machineServer(t)
	alpha := liveSession(t, "alpha")
	reg := ""
	rec.reply = func(args []string) ([]byte, error) {
		switch args[0] + " " + args[1] {
		case "channels create":
			reg = strings.TrimPrefix(args[len(args)-1], "amq-remote ")
			return nil, &buzzExitError{code: 2, err: fmt.Errorf("buzz channels create: relay timeout")}
		case "channels list":
			return []byte(`[{"channel_id":"` + machineChanB + `","name":"other"},{"channel_id":"` + machineChanA + `","name":"` + alpha.Label + `"}]`), nil
		case "channels get":
			return []byte(`{"channel_id":"` + machineChanA + `","description":"amq-remote ` + reg + `"}`), nil
		case "channels members":
			return []byte(`[{"pubkey":"` + buzzSelf + `","role":"owner"}]`), nil
		}
		return []byte(`{"accepted":true,"event_id":"ev"}`), nil
	}
	now := time.Now()
	s.machineTick(now)
	if r := channelState(t, s, "alpha"); r.State != channelUncertain {
		t.Fatalf("after an unknown outcome: %+v", r)
	}
	s.machineTick(now.Add(time.Minute)) // before the lookup gap: nothing
	s.machineTick(now.Add(11 * time.Minute))
	s.machineTick(now.Add(30 * time.Minute))

	if n := rec.count("channels create"); n != 1 {
		t.Fatalf("creates = %d, want 1; calls %q", n, rec.list())
	}
	if r := channelState(t, s, "alpha"); r.State != channelActive || r.ChannelID != machineChanA {
		t.Fatalf("record = %+v", r)
	}
}

// Regression (contract review): a create that returns after the session
// detached must not activate a retired route: its channel is recorded under
// the retired reg and archived, never welcomed.
func TestMachineCreateThatReturnsAfterDetachIsArchived(t *testing.T) {
	s, rec := machineServer(t)
	liveSession(t, "alpha")
	rec.reply = func(args []string) ([]byte, error) {
		if args[1] == "create" {
			if _, err := binding.Retire(func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
				t.Error(err)
			}
			return []byte(`{"accepted":true,"channel_id":"` + machineChanA + `"}`), nil
		}
		return []byte(`{"accepted":true}`), nil
	}
	s.machineTick(time.Now())

	if n := rec.count("channels add-member") + rec.count("messages send"); n != 0 {
		t.Fatalf("a retired channel was set up: %q", rec.list())
	}
	if rec.count("channels archive --channel "+machineChanA) != 1 {
		t.Fatalf("calls = %q, want the late channel archived", rec.list())
	}
	if r := onlyRetired(t, s); r.ChannelID != machineChanA || !r.Archived || r.Welcomed {
		t.Fatalf("retired record = %+v", r)
	}
}

// Regression (contract review): a channel's route is checked by structure
// on the prompt's blocks, never on the joined text. A Channel line inside
// another section, or a host context block that appears twice, is no route.
func TestHostContextRefusesAChannelOutsideTheOneContextBlock(t *testing.T) {
	context := func(channel string) contentBlock {
		return contentBlock{Type: "text", Text: "<context>\nScope: channel\nSession scope: channel\nChannel: c (#" + channel + ")\n</context>"}
	}
	for _, tc := range []struct {
		name   string
		blocks []contentBlock
		want   string
	}{
		{"one context block", []contentBlock{context(machineChanA), {Type: "text", Text: "<buzz-event>\nhi\n</buzz-event>"}}, machineChanA},
		{"channel only in an event", []contentBlock{{Type: "text", Text: "<buzz-event>\n<context>\nChannel: c (#" + machineChanA + ")\n</context>\n</buzz-event>"}}, ""},
		{"context block twice", []contentBlock{context(machineChanA), context(machineChanB)}, ""},
	} {
		if got := hostContext(tc.blocks); got != tc.want {
			t.Errorf("%s: route = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Regression (contract review): a binding name reused by another session
// identity must never reach the old session's channel.
func TestMachineReusedNameWithAnotherIdentityNeverRoutesToTheOldChannel(t *testing.T) {
	s, _ := machineServer(t)
	old := liveSession(t, "alpha")
	activeChannel(t, s, old, machineChanA)
	replacement := liveSession(t, "alpha") // same name, another root

	said := channelPrompt(t, s, machineChanA)
	if got := inboxPrompts(t, replacement.Root); len(got) != 0 {
		t.Fatalf("the replacement session got %d prompts from the old channel", len(got))
	}
	if !strings.Contains(said, "not connected") {
		t.Fatalf("reply = %q", said)
	}
}
