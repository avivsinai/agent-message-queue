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

// Two pi seats with one handle in two project roots, shared through a
// third root, get two targets, each named by its place, and each config
// carries its own root.
func TestPiSeatsOfTwoProjectsShareThroughOneRoot(t *testing.T) {
	link := "/machine-root"
	cand := registry.Candidate{Kind: "pi", Target: "pi:amit", Config: pi.Config("amit", ""), NativeSession: "sess-1"}
	seatRoot1 := filepath.Join(canonicalTempDir(t), "alpha", ".agent-mail", "s1")
	seatRoot2 := filepath.Join(canonicalTempDir(t), "beta", ".agent-mail", "s1")

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
	// A root spelled with a trailing slash is the same session root:
	// the target does not change, and the adapter config is clean.
	trailing, _, err := piThrough(cand, seatRoot1+string(filepath.Separator), link)
	if err != nil {
		t.Fatal(err)
	}
	if trailing.Target != out1.Target {
		t.Fatalf("target after cleaned root changed: %q vs %q", trailing.Target, out1.Target)
	}
	var cfg2 map[string]string
	if err := json.Unmarshal(trailing.Config, &cfg2); err != nil {
		t.Fatal(err)
	}
	if cfg2["root"] != seatRoot1 {
		t.Fatalf("config root with a trailing slash = %q; want %q", cfg2["root"], seatRoot1)
	}
}

// A pi seat that a link's root drives is found from the seat's own root,
// so a plain native attach there is refused.
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

// Detach by seat: selfNativeMatch finds the seat from the share, not the
// candidate's place name, so a place-named target detaches as the same pi seat.
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
	removed, _, err := binding.Detach(match, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != name {
		t.Fatalf("detach removed %v; want %s", removed, name)
	}
}
