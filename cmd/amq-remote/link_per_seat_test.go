package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// S7#6: ls.bindings takes nonEmpty(s.Project, b.Cwd), so a place-named pi
// share still names the seat's own working directory when its session
// reports no project. No new wire field is introduced.
func TestLSBindingsProjectIsCwdWhenSessionReportsNoProject(t *testing.T) {
	root := canonicalTempDir(t)
	stateDir := filepath.Join(root, stateDirName)
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ep := core.New(core.Config{Store: store})
	defer func() { _ = ep.Close() }()

	dir := root
	seatRoot := filepath.Join(dir, "queue")
	cand, seat, err := piThrough(registry.Candidate{Kind: "pi", Target: "pi:amit", Config: pi.Config("amit", ""), NativeSession: "sess-1"}, seatRoot, root)
	if err != nil {
		t.Fatal(err)
	}
	native := cand.Target
	cwd := filepath.Base(seatRoot)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "bindings.json"))
	if err := binding.WriteNamed(binding.Binding{Root: root, Target: cand.Target, NativeSession: native,
		Seat: seat, Cwd: cwd, Name: "p1"}); err != nil {
		t.Fatal(err)
	}
	ls := &linkSet{root: root, stateDir: stateDir, ep: ep, running: map[string]*linkRun{}, stderr: os.Stdout}
	entry := manifest.LinkShare{Binding: "p1"}
	ls.running["ex"] = &linkRun{link: manifest.Link{Name: "ex", Shares: []manifest.LinkShare{entry}}}
	ep.Register(fake.New(cand.Target, "sess-1"))
	pin := linkio.Pin{DeviceName: "dev"}
	out := ls.bindings(ls.running["ex"], pin)
	if len(out) != 1 {
		t.Fatalf("expected one current active binding, got %d", len(out))
	}
	if out[0].Labels.Project != cwd {
		t.Fatalf("Labels.Project = %q; want seat Cwd %q", out[0].Labels.Project, cwd)
	}
}
