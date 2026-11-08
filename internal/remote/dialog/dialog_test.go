package dialog

import (
	"path/filepath"
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
