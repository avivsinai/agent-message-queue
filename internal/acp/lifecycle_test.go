package acp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// A pinned agent archives itself when its binding is removed
// (/amq-remote off) and unarchives itself when the same session binds again.
func TestPinnedAgentArchivesAndReturnsWithItsBinding(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	bound := binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha"}
	if err := binding.WriteNamed(bound); err != nil {
		t.Fatal(err)
	}
	savedIdentity, savedSelf, savedOwner, savedCall := buzzIdentity, buzzSelf, buzzOwner, buzzCall
	t.Cleanup(func() { buzzIdentity, buzzSelf, buzzOwner, buzzCall = savedIdentity, savedSelf, savedOwner, savedCall })
	recordPosts(t)
	t.Setenv("BUZZ_PRIVATE_KEY", strings.Repeat("1", 64))
	captureBuzzIdentity()
	if len(buzzSelf) != 64 {
		t.Fatalf("own public key = %q", buzzSelf)
	}
	var calls []string
	buzzCall = func(_ time.Duration, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	s := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: canonicalTempDir(t)}, "test")

	s.lifecycleTick(time.Now()) // bound: nothing to change
	if _, err := binding.RemoveMatching(func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
		t.Fatal(err)
	}
	s.lifecycleTick(time.Now())
	s.lifecycleTick(time.Now()) // already archived: no second request
	if err := binding.WriteNamed(bound); err != nil {
		t.Fatal(err)
	}
	s.lifecycleTick(time.Now())

	want := []string{
		"agents archive " + buzzSelf + " --reason retired",
		"users set-status --text session ended",
		"agents unarchive " + buzzSelf + " --reason returned",
		"users set-status --clear",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("buzz calls = %q\nwant %q", calls, want)
	}
}

// Regression: two agents pinned to one session shared one marker, so
// each reset the other's state. Archive repeated on every tick, and
// unarchive never ran when the binding came back.
func TestTwoAgentsOnOnePinEachChangeOnce(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	bound := binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha"}
	if err := binding.WriteNamed(bound); err != nil {
		t.Fatal(err)
	}
	savedSelf, savedCall := buzzSelf, buzzCall
	t.Cleanup(func() { buzzSelf, buzzCall = savedSelf, savedCall })
	recordPosts(t)
	var calls []string
	buzzCall = func(_ time.Duration, args ...string) ([]byte, error) {
		calls = append(calls, buzzSelf[:1]+" "+strings.Join(args[:2], " "))
		return nil, nil
	}
	state := canonicalTempDir(t)
	a := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: state}, "test")
	b := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: state}, "test")
	ticks := func() {
		buzzSelf = strings.Repeat("a", 64)
		a.lifecycleTick(time.Now())
		buzzSelf = strings.Repeat("b", 64)
		b.lifecycleTick(time.Now())
	}

	if _, err := binding.RemoveMatching(func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		ticks()
	}
	if err := binding.WriteNamed(bound); err != nil {
		t.Fatal(err)
	}
	ticks()

	want := []string{
		"a agents archive", "a users set-status", "b agents archive", "b users set-status",
		"a agents unarchive", "a users set-status", "b agents unarchive", "b users set-status",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("buzz calls = %q\nwant %q", calls, want)
	}
}

// Desktop's Create agent runs one agent as N identical workers (parallelism
// 10), all with one Buzz identity. Each worker read
// the shared marker and archived the agent once more; only the lifecycle
// leader may act, and another worker takes over when the leader stops.
func TestIdenticalWorkersChangeTheAgentOnce(t *testing.T) {
	if !lock.AdvisoryLockAvailable() {
		t.Skip("no advisory lock on this platform; amq-acp runs one worker")
	}
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	bound := binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha"}
	if err := binding.WriteNamed(bound); err != nil {
		t.Fatal(err)
	}
	savedSelf, savedCall := buzzSelf, buzzCall
	t.Cleanup(func() { buzzSelf, buzzCall = savedSelf, savedCall })
	recordPosts(t)
	buzzSelf = strings.Repeat("a", 64)
	state := canonicalTempDir(t)
	a := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: state}, "test")
	b := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: state}, "test")
	t.Cleanup(a.resign)
	t.Cleanup(b.resign)
	var calls []string
	reentered := false
	buzzCall = func(_ time.Duration, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args[:2], " "))
		if args[1] == "archive" && !reentered {
			reentered = true
			b.lifecycleTick(time.Now()) // b ticks while a's archive runs
		}
		return nil, nil
	}

	if _, err := binding.RemoveMatching(func(b binding.Binding) bool { return b.Name == "alpha" }); err != nil {
		t.Fatal(err)
	}
	a.lifecycleTick(time.Now())
	a.resign() // a stops: b leads from its next tick
	if err := binding.WriteNamed(bound); err != nil {
		t.Fatal(err)
	}
	b.lifecycleTick(time.Now())

	want := []string{"agents archive", "users set-status", "agents unarchive", "users set-status"}
	if !slices.Equal(calls, want) {
		t.Fatalf("buzz calls = %q\nwant %q", calls, want)
	}
}

// The helper creates its control channel once, adds the owner,
// and asks Desktop for one agent per bound session. Once that agent runs,
// the helper asks nothing more.
func TestHelperDraftsAnAgentForANewSession(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	holder, err := launch.HolderOf(os.Getpid()) // this test is the live session
	if err != nil {
		t.Skip("no holder identity on this platform:", err)
	}
	const label = "claude · project/alpha @ machine"
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha", Holder: holder, Label: label}); err != nil {
		t.Fatal(err)
	}
	savedSelf, savedOwner, savedCall := buzzSelf, buzzOwner, buzzCall
	t.Cleanup(func() { buzzSelf, buzzOwner, buzzCall = savedSelf, savedOwner, savedCall })
	recordPosts(t)
	const control = "0b2c39a1-6f1e-4d0a-9b3a-5c1d2e3f4a5b"
	helperKey, owner := strings.Repeat("c", 64), strings.Repeat("d", 64)
	buzzOwner = owner
	var calls []string
	buzzCall = recordHelperCalls(&calls, control)
	state := canonicalTempDir(t)
	helper := NewServer(Config{RemoteBinding: true, Helper: true, StateDir: state}, "test")
	agent := NewServer(Config{RemoteBinding: true, Pin: "alpha", StateDir: state}, "test")
	now := time.Now()

	buzzSelf = helperKey
	helper.lifecycleTick(now)
	buzzSelf = strings.Repeat("a", 64) // the agent the owner created starts
	agent.lifecycleTick(now)
	buzzSelf = helperKey
	helper.lifecycleTick(now.Add(time.Hour))

	want := []string{
		"channels list --member --visibility private",
		"channels create --name amq-remote @ " + binding.Machine() + " --type stream --visibility private",
		"channels add-member --channel " + control + " --pubkey " + owner + " --role admin",
		"agents draft-create --channel " + control + " --display-name " + label + " --system-prompt AMQ Remote session: alpha",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("buzz calls = %q\nwant %q", calls, want)
	}
}

// Regression: the relay refused Desktop's add of a new session
// agent to the control channel ("only owners/admins may add private-channel
// members"), because the helper had added the owner as a plain member. A
// helper whose marker predates the fix makes the owner an admin of its
// existing channel, without creating another one.
func TestHelperMakesTheOwnerAnAdminOfAnExistingChannel(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	savedSelf, savedOwner, savedCall := buzzSelf, buzzOwner, buzzCall
	t.Cleanup(func() { buzzSelf, buzzOwner, buzzCall = savedSelf, savedOwner, savedCall })
	recordPosts(t)
	const control = "0b2c39a1-6f1e-4d0a-9b3a-5c1d2e3f4a5b"
	buzzSelf, buzzOwner = strings.Repeat("c", 64), strings.Repeat("d", 64)
	var calls []string
	buzzCall = recordHelperCalls(&calls, control)
	helper := NewServer(Config{RemoteBinding: true, Helper: true, StateDir: canonicalTempDir(t)}, "test")
	dir := helper.agentMarkerDir(helperName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"name":"_helper","channel":"` + control + `","owner_added":true,"updated":"2026-10-19T15:48:38Z"}`
	if err := os.WriteFile(filepath.Join(dir, buzzSelf+".json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	helper.lifecycleTick(time.Now())

	want := []string{"channels add-member --channel " + control + " --pubkey " + buzzOwner + " --role admin"}
	if !slices.Equal(calls, want) {
		t.Fatalf("buzz calls = %q\nwant %q", calls, want)
	}
}

// recordHelperCalls records the helper's buzz calls and answers them as a
// relay that accepts: no channel yet, then control as the created channel.
func recordHelperCalls(calls *[]string, control string) func(time.Duration, ...string) ([]byte, error) {
	return func(_ time.Duration, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		switch args[0] + " " + args[1] {
		case "channels list":
			return []byte(`[]`), nil
		case "channels create":
			return []byte(`{"accepted":true,"channel_id":"` + control + `"}`), nil
		case "channels add-member":
			return []byte(`{"accepted":true}`), nil
		}
		return nil, nil
	}
}

// Regression: Desktop shows one draft at a time and drops a draft
// that arrives while one is open. The helper drafted every new session in
// one tick, so only the first opened and the others spent their tries.
func TestHelperDraftsOneSessionPerWindow(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no holder identity on this platform:", err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: name, Holder: holder}); err != nil {
			t.Fatal(err)
		}
	}
	savedSelf, savedOwner, savedCall := buzzSelf, buzzOwner, buzzCall
	t.Cleanup(func() { buzzSelf, buzzOwner, buzzCall = savedSelf, savedOwner, savedCall })
	recordPosts(t)
	buzzSelf, buzzOwner = strings.Repeat("c", 64), strings.Repeat("d", 64)
	var calls []string
	buzzCall = recordHelperCalls(&calls, "0b2c39a1-6f1e-4d0a-9b3a-5c1d2e3f4a5b")
	helper := NewServer(Config{RemoteBinding: true, Helper: true, StateDir: canonicalTempDir(t)}, "test")
	drafts := func() []string {
		var out []string
		for _, c := range calls {
			if strings.HasPrefix(c, "agents draft-create") {
				out = append(out, c[strings.LastIndex(c, ": ")+2:])
			}
		}
		return out
	}
	now := time.Now()
	helper.lifecycleTick(now)
	helper.lifecycleTick(now.Add(time.Minute))
	if got := drafts(); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("drafts in the first window = %q; want only alpha", got)
	}
	helper.lifecycleTick(now.Add(helperRequestGap + time.Minute))
	if got := drafts(); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("drafts after the next window = %q; want beta next", got)
	}
}
