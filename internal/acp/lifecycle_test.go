package acp

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// Design S3: a pinned agent archives itself when its binding is removed
// (/amq-remote off) and unarchives itself when the same session binds again.
func TestPinnedAgentArchivesAndReturnsWithItsBinding(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	bound := binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha"}
	if err := binding.WriteNamed(bound); err != nil {
		t.Fatal(err)
	}
	savedIdentity, savedSelf, savedOwner, savedCall := buzzIdentity, buzzSelf, buzzOwner, buzzCall
	t.Cleanup(func() { buzzIdentity, buzzSelf, buzzOwner, buzzCall = savedIdentity, savedSelf, savedOwner, savedCall })
	t.Setenv("BUZZ_PRIVATE_KEY", strings.Repeat("1", 64))
	captureBuzzIdentity()
	if len(buzzSelf) != 64 {
		t.Fatalf("own public key = %q", buzzSelf)
	}
	var calls []string
	buzzCall = func(_ time.Duration, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
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

// Review of S3 P1-1: two agents pinned to one session shared one marker, so
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
	var calls []string
	buzzCall = func(_ time.Duration, args ...string) error {
		calls = append(calls, buzzSelf[:1]+" "+strings.Join(args[:2], " "))
		return nil
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
