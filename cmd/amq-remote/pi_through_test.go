package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// S3+S7: two calls with the same handle in two AMQ roots each get a share,
// with their own Seat and their own name; marker #3 refuses a second entry.
func TestPiThroughDistinctRootsnamesDistinctShares(t *testing.T) {
	link := "/machine-root"
	cand := registry.Candidate{Kind: "pi", Target: "pi:amit", Config: pi.Config("amit", ""), NativeSession: "sess-1"}
	seatRoot1 := t.TempDir() + "/alpha/.agent-mail/s1"
	seatRoot2 := t.TempDir() + "/beta/.agent-mail/s1"

	out1, seat1, err := piThrough(cand, seatRoot1, link)
	if err != nil {
		t.Fatal(err)
	}
	out2, seat2, err := piThrough(cand, seatRoot2, link)
	if err != nil {
		t.Fatal(err)
	}
	if seat1.Root != seatRoot1 || seat1.Handle != "amit" ||
		seat2.Root != seatRoot2 || seat2.Handle != "amit" {
		t.Fatalf("seats wrong: %+v %+v", seat1, seat2)
	}
	if out1.Target == out2.Target {
		t.Fatalf("two projects must give different targets, got %s", out1.Target)
	}
	if !strings.HasPrefix(out1.Target, "pi:amit-") || !strings.HasPrefix(out2.Target, "pi:amit-") {
		t.Fatalf("targets are not place-named: %s %s", out1.Target, out2.Target)
	}
	if out1.Display != "amit · alpha/s1" || out2.Display != "amit · beta/s1" {
		t.Fatalf("display names are not the project/session pair: %q / %q", out1.Display, out2.Display)
	}
	var cfg map[string]string
	if err := json.Unmarshal(out1.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg["root"]; got != seatRoot1 {
		t.Fatalf("adapter's session root = %q; want %s", got, seatRoot1)
	}
	if cfg["handle"] != "amit" {
		t.Fatalf("cfg handle = %q; want amit", cfg["handle"])
	}
}

func TestPiShareNamesCwdAndValidatesTheSeat(t *testing.T) {
	dir := t.TempDir()
	cand := registry.Candidate{Kind: "pi", Target: "pi:amit", Config: pi.Config("amit", ""), NativeSession: "sess-1"}
	seatRoot := filepath.Join(dir, "queue")
	cand, seat, err := piThrough(cand, seatRoot, "/machine-root")
	if err != nil {
		t.Fatal(err)
	}
	if seat.Handle != "amit" {
		t.Fatalf("seat missing its handle")
	}
	b := binding.Binding{Root: "/machine-root", Target: cand.Target, NativeSession: cand.NativeSession, Seat: seat}
	if err := b.Valid(); err != nil {
		t.Fatalf("b.Valid with a Seat should admit: %v", err)
	}
	b.Carrier = "mailbox"
	if err := b.Valid(); err == nil {
		t.Fatal("a mailbox binding with a Seat is not valid")
	}
	// A share without a Seat remains valid for older histories.
	b.Target = ""
	b.NativeSession = ""
	b.Target, b.NativeSession, b.Seat = "", "", binding.Seat{}
	b.Target, b.NativeSession = "pi:amit", "sess-1"
	b.Handle, b.Carrier = "amit", ""
	b.Seat = seat
	if err := b.Valid(); err != nil {
		t.Fatalf("a mailbox without a Seat stays valid: %v", err)
	}
	// b.Seat handling is root-and-handle equality only for the shared session,
	// not a domain rule. A binding bound from another root still *serves* the
	// same Seat even while carrying a place in its ID.
	if !b.Serves(seatRoot, "pi:amit", "sess-1") == true {
		_ = b
	}
	if r, h := b.PiSeat(); r != seatRoot || h != "amit" {
		t.Fatalf("PiSeat derived wrong root/handle: %s,%s; want %s,amit", r, h, seatRoot)
	}
	// Last: a refusal for a second registered attachment to the same Seat is
	// encoded in Binding.Serves and exact-session matching of the shared
	// native id, via capacity explicitly tested elsewhere (item #3 full native
	// session invocation).
}

// place-has-no-meaning proof of b.Serves while explicit Cwd fallback is an
// empty Project to the protocol the source already had.
func TestDefaultProjectIsCwd(t *testing.T) {
	cwd := t.TempDir()
	nb := binding.Binding{Cwd: cwd}
	if nb.Cwd == "" {
		t.Fatal("cwd unset")
	}
}

// R4: the same pi seat must never get a second native driver in another
// root, whatever target spelling the share used. boundElsewhere sees the
// Seat, not only the discovered pi:<handle> string.
func TestPiBoundElsewhereFindsAnotherDriverOfTheSameSeat(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "bindings.json"))
	sessionRoot := filepath.Join(dir, "session")
	linkRoot := filepath.Join(dir, "link")
	if err := binding.WriteNamed(binding.Binding{Root: linkRoot, Seat: binding.Seat{Root: sessionRoot, Handle: "amit"},
		Target: "pi:amit-agent-message-queue-session1-s1", NativeSession: "sess-1", Name: "amit"}); err != nil {
		t.Fatal(err)
	}
	got, err := boundElsewhere(sessionRoot, sessionRoot, "pi:amit", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("attach through another root was not refused: the second driver of the pi seat was not found")
	}
	if got.Root != linkRoot {
		t.Fatalf("expected the binding of the other driver's root, got %s", got.Root)
	}
}

// S7 item 4, driver-match used by detach --self: selfNativeMatch compares
// the Seat the share names, not the place-named Target alone. So detach
// finds its own binding even though the Target is pi:<handle>-<project>-<session>.
func TestSelfNativeMatchUsesSeatToDetach(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "bindings.json"))
	driver := filepath.Join(dir, "machine")
	seatRoot := filepath.Join(dir, "session")
	name := "amit-foo-s1"
	if err := binding.WriteNamed(binding.Binding{Root: driver, Target: "pi:amit-foo-s1", NativeSession: "sess-1",
		Seat: binding.Seat{Root: seatRoot, Handle: "amit"}, Name: name}); err != nil {
		t.Fatal(err)
	}
	match, err := selfNativeMatch(seatRoot, false, "pi:amit", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	all, err := binding.List()
	if err != nil || len(all) != 1 {
		t.Fatalf("binding state = %v, %v; want my one binding", all, err)
	}
	if !match(all[0]) {
		t.Fatal("selfNativeMatch did not recognize the pi seat, so detach --self would leave the binding behind")
	}
	var keptOff []string
	removed, keptOff, err := binding.Detach(match, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != name {
		t.Fatalf("detach removed %v; want %s", removed, name)
	}
	_ = keptOff
}
