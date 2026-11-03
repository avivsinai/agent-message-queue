package dialog

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
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
	name, err := FindBinding(root, "claude", "", nil)
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
	if got, ok, _ := ReadAnswer(dir, req.ID); !ok || got.Option != 2 {
		t.Fatalf("ReadAnswer = %+v, %v", got, ok)
	}
	if won, err := Resolve(dir, Resolved{ID: req.ID, Outcome: OutcomeDenied, By: ByBuzz}); !won || err != nil {
		t.Fatalf("Resolve = %v, %v", won, err)
	}
	if won, _ := Resolve(dir, Resolved{ID: req.ID, Outcome: OutcomeTerminal, By: ByTerminal}); won {
		t.Fatal("a second resolution won")
	}
	if r, ok, _ := ReadResolved(dir, req.ID); !ok || r.Outcome != OutcomeDenied {
		t.Fatalf("ReadResolved = %+v, %v", r, ok)
	}
	if open, _ := Requests(dir); len(open) != 0 {
		t.Fatalf("a resolved request is still open: %+v", open)
	}
}

// A binding matches a process the seam runs under only by that process's
// whole identity, or, with no recorded holder, by its native Claude
// target's pid.
//
// Regression: code review of the per-session channel change found that a
// match on the pid alone let a reused pid capture another session's dialogs,
// and that a native Claude binding with only a target pid never matched.
func TestFindBindingMatchesTheWholeHolder(t *testing.T) {
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(binding.EnvPath, filepath.Join(store, "binding.json"))
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ancestors := launch.Ancestors()
	if len(ancestors) == 0 {
		t.Skip("no process ancestry on this platform")
	}
	proc := ancestors[0]
	// Another process under the same pid, recorded without the boot that
	// would prove it gone: its liveness is unknown, not ended.
	reused := proc
	reused.ProcessStart, reused.BootID = reused.ProcessStart+"0", ""
	if err := binding.WriteNamed(binding.Binding{Name: "reused", Root: root, Target: "codex:x", NativeSession: "s-a", Holder: reused, BoundAt: "2026-11-03T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if name, err := FindBinding("", "", "s-new", []launch.HolderIdentity{proc}); err != nil || name != "" {
		t.Fatalf("FindBinding = %q, %v; a reused pid must not match", name, err)
	}
	if err := binding.WriteNamed(binding.Binding{Name: "target", Root: root, Target: "claude:" + strconv.Itoa(proc.PID), NativeSession: "s-b", BoundAt: "2026-11-03T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if name, err := FindBinding("", "", "s-new", []launch.HolderIdentity{proc}); err != nil || name != "target" {
		t.Fatalf("FindBinding = %q, %v; want the binding of the target pid", name, err)
	}
}

// Dropping a session record whose process is gone leaves a record that
// another session wrote under the same name meanwhile.
//
// Regression: code review of the per-session channel change found that the
// machine agent's cleanup removed whatever record held the name, so a
// session that had just taken the name over lost its record.
func TestDropSessionKeepsANewerSessionsRecord(t *testing.T) {
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(binding.EnvPath, filepath.Join(store, "binding.json"))
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	gone := holder
	gone.ProcessStart += "0"
	name, err := RecordSession(SessionRecord{Harness: "claude", Session: "s-new", Label: "claude · proj", Holder: holder}, "claude-proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := DropSession(name, gone); err != nil {
		t.Fatal(err)
	}
	if r, ok, err := ReadSession(name); err != nil || !ok || r.Session != "s-new" {
		t.Fatalf("record = %+v, %v, %v; want the newer session's record kept", r, ok, err)
	}
	if err := DropSession(name, holder); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ReadSession(name); ok {
		t.Fatal("the record of the gone holder was not dropped")
	}
}
