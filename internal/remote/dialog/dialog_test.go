package dialog

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

func TestDialogRoundTripFirstResolutionWins(t *testing.T) {
	// The binding store refuses a symlinked path, and the macOS temp dir is
	// under the /var symlink (macOS CI failed here).
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(binding.EnvPath, filepath.Join(store, "binding.json"))
	root := t.TempDir()
	if err := binding.WriteNamed(binding.Binding{Name: "proj", Carrier: binding.CarrierMailbox, Root: root, Handle: "claude", BoundAt: "2026-11-02T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	name, err := FindBinding(root, "claude", "")
	if err != nil || name != "proj" {
		t.Fatalf("FindBinding = %q, %v", name, err)
	}
	// Review of agent-message-queue-eqzq: a matching native binding can
	// sort before the mailbox binding that carries the dialog's channel.
	const session = "s1"
	if err := binding.WriteNamed(binding.Binding{Name: "native", Carrier: binding.CarrierNative, Root: root,
		Target: "pi:agent", Harness: "pi", NativeSession: session}); err != nil {
		t.Fatal(err)
	}
	if got, err := FindBinding(root, "claude", session); err != nil || got != name {
		t.Fatalf("FindBinding with both matches = %q, %v; want %q", got, err, name)
	}
	dir, err := DirFor(name)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{ID: NewID(), Harness: "claude", Tool: "Bash", Text: "$ ls", OpenedAt: time.Now(),
		Options: []Option{{N: 1, Label: "Allow once", Value: "allow"}, {N: 2, Label: "Deny", Value: "deny", Deny: true}}}
	if err := Open(dir, req); err != nil {
		t.Fatal(err)
	}
	open, err := Requests(dir)
	if err != nil || len(open) != 1 || open[0].ID != req.ID {
		t.Fatalf("Requests = %+v, %v", open, err)
	}
	a, ok := ParseAnswer(open[0], "❌")
	if !ok || a.Option != 2 {
		t.Fatalf("ParseAnswer(❌) = %+v, %v", a, ok)
	}
	if won, err := WriteAnswer(dir, a); !won || err != nil {
		t.Fatalf("WriteAnswer = %v, %v", won, err)
	}
	if won, _ := WriteAnswer(dir, Answer{ID: req.ID, Option: 1}); won {
		t.Fatal("a second answer replaced the first")
	}
	if phase, _, got, _, err := Lookup(dir, req.ID); phase != PhaseAnswered || got == nil || got.Option != 2 || err != nil {
		t.Fatalf("Lookup = %s, %+v, %v; want answered with option 2", phase, got, err)
	}
	if won, err := Resolve(dir, Resolved{ID: req.ID, Outcome: OutcomeDenied, By: ByBuzz}); !won || err != nil {
		t.Fatalf("Resolve = %v, %v", won, err)
	}
	if won, _ := Resolve(dir, Resolved{ID: req.ID, Outcome: OutcomeTerminal, By: ByTerminal}); won {
		t.Fatal("a second resolution won")
	}
	if phase, _, _, r, err := Lookup(dir, req.ID); phase != PhaseResolved || r == nil || r.Outcome != OutcomeDenied || err != nil {
		t.Fatalf("Lookup = %s, %+v, %v; want resolved denied", phase, r, err)
	}
	if open, _ := Requests(dir); len(open) != 0 {
		t.Fatalf("a resolved request is still open: %+v", open)
	}
}

// Review of the dialog phase lookup: a terminal resolution must win over a
// leftover answer that cannot be read; before, Lookup returned the parse
// error and a waiting hook kept waiting.
func TestLookupResolutionWinsOverAnUnreadableAnswer(t *testing.T) {
	dir, id := t.TempDir(), NewID()
	if err := Open(dir, Request{ID: id, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(dir, Resolved{ID: id, Outcome: OutcomeTerminal, By: ByTerminal}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "answers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "answers", id+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if phase, _, _, r, err := Lookup(dir, id); phase != PhaseResolved || r == nil || err != nil {
		t.Fatalf("Lookup = %q, %+v, %v; want the terminal resolution", phase, r, err)
	}
}

// R1: the baseline is a plain listing of the requests directory — ids of the
// files on disk, an answered-but-unresolved dialog included, no record read
// and none filtered. A missing directory is the empty list; another read
// error is returned so the caller can store no list.
func TestOpenRequestIDsListsTheDirectoryPlainly(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(t.TempDir(), "binding.json"))
	dir, err := DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := OpenRequestIDs("alpha")
	if err != nil || len(ids) != 0 {
		t.Fatalf("missing directory = %v, %v; want empty, nil", ids, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "requests"), 0o700); err != nil {
		t.Fatal(err)
	}
	req := Request{ID: NewID(), Harness: "claude", OpenedAt: time.Now().UTC()}
	if err := Open(dir, req); err != nil {
		t.Fatal(err)
	}
	// An answered dialog with no resolution is still on disk.
	if _, err := WriteAnswer(dir, Answer{ID: req.ID, Option: 1, By: ByBuzz}); err != nil {
		t.Fatal(err)
	}
	ids, err = OpenRequestIDs("alpha")
	if err != nil || len(ids) != 1 || ids[0] != req.ID {
		t.Fatalf("listing = %v, %v; want [%s]", ids, err, req.ID)
	}
	// A stray non-request file and a subdirectory are not ids.
	if err := os.WriteFile(filepath.Join(dir, "requests", "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "requests", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A request-named directory entry of a nonregular type is still a name
	// Requests could later select, so the baseline lists it too.
	dirID := NewID()
	if err := os.Mkdir(filepath.Join(dir, "requests", dirID+".json"), 0o700); err != nil {
		t.Fatal(err)
	}
	ids, err = OpenRequestIDs("alpha")
	if err != nil || len(ids) != 2 {
		t.Fatalf("listing after strays = %v, %v; want the request and the request-named directory", ids, err)
	}
	sort.Strings(ids)
	want := []string{req.ID, dirID}
	sort.Strings(want)
	if ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("listing = %v, %v; want {%s %s}", ids, err, want[0], want[1])
	}
	// An unlistable directory returns the error.
	if err := os.Chmod(filepath.Join(dir, "requests"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "requests"), 0o700) })
	if _, err := OpenRequestIDs("alpha"); err == nil {
		t.Fatal("an unreadable directory returned no error")
	}
}
