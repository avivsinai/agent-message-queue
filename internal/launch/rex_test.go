package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type fakeRexBlock struct {
	ID      string `json:"block_id"`
	Label   string `json:"label"`
	Creator string `json:"creator_name"`
}

type fakeRexLayer struct {
	Kind   string         `json:"kind"`
	Blocks []fakeRexBlock `json:"blocks"`
}

type fakeRexWindow struct {
	ID     string         `json:"window_id"`
	Label  string         `json:"label"`
	Layers []fakeRexLayer `json:"layers"`
}

type fakeRexSession struct {
	ID      string          `json:"session_id"`
	Windows []fakeRexWindow `json:"windows"`
}

type fakeRex struct {
	t          *testing.T
	instance   string
	outside    bool
	sessions   []fakeRexSession
	calls      [][]string
	seq        int
	fail       string
	failure    error
	loginPolls int
	polls      map[string]int
	ready      map[string]bool
}

func (f *fakeRex) nextID(kind string) string {
	f.seq++
	return fmt.Sprintf("%s:019c5a10-75d8-7eef-8db7-%012x", kind, f.seq)
}

func (f *fakeRex) emit(value any) (string, error) {
	raw, err := json.Marshal(value)
	return string(raw), err
}

func (f *fakeRex) run(_ context.Context, args ...string) (string, error) {
	f.t.Helper()
	if len(args) == 0 || slices.Contains(args, "--autostart=false") {
		f.t.Fatalf("run seam must receive command args without runner flags: %v", args)
	}
	f.calls = append(f.calls, slices.Clone(args))
	op := args[0]
	if op == "window" || op == "block" {
		op += " " + args[1]
	}
	if op == f.fail {
		return "", f.failure
	}
	flag := func(name string) string {
		for i, arg := range args {
			if arg == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	assert := func(want ...string) {
		f.t.Helper()
		if !reflect.DeepEqual(args, want) {
			f.t.Fatalf("rex args = %v, want %v", args, want)
		}
	}
	session := flag("-s")
	block := flag("-b")
	switch op {
	case "whoami":
		assert("whoami", "--json")
		value := map[string]any{"instance_id": f.instance, "api_revision": 24, "endpoint": "unix:/fake/rex", "host_id": "fake-host", "rex_version": "0.1.0-preview.25"}
		if !f.outside {
			value["session_id"], value["window_id"], value["block_id"] = f.sessions[0].ID, f.sessions[0].Windows[0].ID, f.sessions[0].Windows[0].Layers[0].Blocks[0].ID
		}
		return f.emit(value)
	case "ls":
		assert("ls", "--json")
		sessions := make([]map[string]string, 0, len(f.sessions))
		for _, s := range f.sessions {
			sessions = append(sessions, map[string]string{"session_id": s.ID, "label": "existing"})
		}
		return f.emit(map[string]any{"sessions": sessions})
	case "window ls":
		assert("window", "ls", "-s", session, "--json")
		for _, s := range f.sessions {
			if s.ID == session {
				return f.emit(s)
			}
		}
	case "window new":
		if len(args) != 9 {
			f.t.Fatalf("window new args = %v", args)
		}
		assert("window", "new", "-s", session, args[4], "-c", args[6], "--focus=false", "--json")
	case "split":
		if len(args) != 12 {
			f.t.Fatalf("split args = %v", args)
		}
		direction := args[5]
		if direction != "--split=right" && direction != "--split=below" {
			f.t.Fatalf("split direction = %q", direction)
		}
		assert("split", "-s", session, "-b", block, direction, "--focus=false", "-c", args[8], "--label", args[10], "--json")
	case "block rename":
		if len(args) != 6 {
			f.t.Fatalf("block rename args = %v", args)
		}
		assert("block", "rename", "-s", session, args[4], args[5])
		block = args[4]
	case "block call":
		assert("block", "call", "-s", session, "-b", block, "process")
	case "send":
		assert("send", "-s", session, "-b", block, "--", args[6])
		for _, s := range f.sessions {
			for _, w := range s.Windows {
				if !strings.HasPrefix(w.Label, "amq-") {
					continue
				}
				for _, l := range w.Layers {
					for _, b := range l.Blocks {
						if !f.ready[b.ID] {
							f.t.Fatalf("send before every pane had a shell: %v", f.calls)
						}
					}
				}
			}
		}
	case "send-key":
		assert("send-key", "-s", session, "-b", block, "enter")
	case "focus", "block close":
		if op == "focus" {
			assert("focus", "-s", session, session)
		} else {
			assert("block", "close", "-s", session, session)
		}
		block = session
	case "window close":
		assert("window", "close", "-s", session, args[4])
	default:
		f.t.Fatalf("unknown rex call: %v", args)
	}
	for si := range f.sessions {
		s := &f.sessions[si]
		if op == "window new" && s.ID == session {
			w := fakeRexWindow{ID: f.nextID("window"), Label: args[4], Layers: []fakeRexLayer{{Kind: "tiled", Blocks: []fakeRexBlock{{ID: f.nextID("block"), Creator: rexTerminalCreator}}}}}
			s.Windows = append(s.Windows, w)
			return f.emit(map[string]any{"window_id": w.ID, "block_ids": []string{w.Layers[0].Blocks[0].ID}, "revision": 1})
		}
		if op != "focus" && op != "block close" && s.ID != session {
			continue
		}
		for wi := range s.Windows {
			w := &s.Windows[wi]
			if op == "window close" && w.ID == args[4] {
				s.Windows = slices.Delete(s.Windows, wi, wi+1)
				return "", nil
			}
			for li := range w.Layers {
				l := &w.Layers[li]
				for bi := range l.Blocks {
					b := &l.Blocks[bi]
					if b.ID != block {
						continue
					}
					switch op {
					case "split":
						added := fakeRexBlock{ID: f.nextID("block"), Label: args[10], Creator: rexTerminalCreator}
						l.Blocks = append(l.Blocks, added)
						return f.emit(map[string]any{"anchor_id": block, "block_ids": []string{added.ID}, "split_ids": []string{f.nextID("split")}, "revision": 2})
					case "block rename":
						b.Label = args[5]
					case "block call":
						f.polls[block]++
						name := "login"
						if f.polls[block] > f.loginPolls {
							name = "zsh"
							f.ready[block] = true
						}
						return f.emit(map[string]any{"foreground": map[string]any{"pid": 123, "name": name}})
					case "block close":
						l.Blocks = slices.Delete(l.Blocks, bi, bi+1)
						if len(l.Blocks) == 0 {
							s.Windows = slices.Delete(s.Windows, wi, wi+1)
						}
					}
					return "", nil
				}
			}
		}
	}
	return "", fmt.Errorf("Error: target not found: %v", args)
}

func newFakeRexBackend(t *testing.T) (*RexBackend, *fakeRex) {
	t.Helper()
	f := &fakeRex{t: t, instance: "019c5a10-75d8-7eef-8db7-eeeeeeeeeeee", failure: errors.New("injected failure"), polls: make(map[string]int), ready: make(map[string]bool)}
	for range 2 {
		f.sessions = append(f.sessions, fakeRexSession{ID: f.nextID("session"), Windows: []fakeRexWindow{{ID: f.nextID("window"), Label: "existing", Layers: []fakeRexLayer{{Kind: "tiled", Blocks: []fakeRexBlock{{ID: f.nextID("block"), Creator: rexTerminalCreator}, {ID: f.nextID("block"), Creator: "other.app"}}}}}}})
	}
	b := NewRexBackend("")
	b.run = f.run
	b.hostname = func() (string, error) { return "host.test", nil }
	b.sleep = func(context.Context, time.Duration) error { return nil }
	b.healthTimeout = time.Second
	return b, f
}

func rexCreateRequest(t *testing.T) CreateRequest {
	t.Helper()
	_, root := harnessRoot(t)
	project := t.TempDir()
	amq := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(amq, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70e7a5"
	return CreateRequest{Root: root, ProjectRoot: project, Session: "collab", AMQPath: amq,
		Plan: Plan{Version: PlanVersion, Agents: []AgentPlan{
			{Handle: "claude", Argv: []string{"claude"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7a6"},
			{Handle: "codex", Argv: []string{"codex"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7a7"},
		}}}
}

func TestRexBackendLifecycleAndRecovery(t *testing.T) {
	b, f := newFakeRexBackend(t)
	req := rexCreateRequest(t)
	detect := b.Detect()
	if !detect.Available || detect.Profile.Identity() != "rex/darwin/v1" || detect.InstanceIdentity != rexInstancePrefix+f.instance {
		t.Fatalf("Detect = %#v", detect)
	}
	if err := detect.Validate(); err != nil {
		t.Fatal(err)
	}
	created, err := b.Create(req)
	if err != nil {
		t.Fatal(err)
	}
	if created.Outcome != OutcomeCreated || len(created.Binding.Resources.Resources) != 3 {
		t.Fatalf("Create = %#v", created)
	}
	windowID, nonce, err := parseRexWindowResource(created.Binding)
	if err != nil || nonce != req.Plan.Agents[0].LaunchNonce {
		t.Fatalf("window resource = %q, %q, %v", windowID, nonce, err)
	}
	blocks, err := rexBoundBlocks(created.Binding)
	if err != nil || len(blocks) != 2 {
		t.Fatalf("pane resources = %v, %v", blocks, err)
	}
	for i, agent := range req.Plan.Agents {
		if created.Binding.Resources.Resources[i+1] != (ResourceIdentity{OpaqueID: rexBlockPrefix + blocks[i], Agent: agent.Handle}) {
			t.Fatalf("resources = %#v", created.Binding.Resources)
		}
	}
	inspected, err := b.Inspect(InspectRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || inspected.Status != InspectPresent {
		t.Fatalf("Inspect = %#v, %v", inspected, err)
	}
	focused, err := b.Focus(FocusRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || focused.Outcome != OutcomeAttached {
		t.Fatalf("Focus = %#v, %v", focused, err)
	}
	if !reflect.DeepEqual(f.calls[len(f.calls)-1], []string{"focus", "-s", "block:" + blocks[0], "block:" + blocks[0]}) {
		t.Fatalf("focus call = %v", f.calls[len(f.calls)-1])
	}
	journal := LaunchJournal{Phase: JournalCreated, ProjectIdentity: req.ProjectRoot, Session: req.Session, Plan: req.Plan,
		LaunchNonce: nonce, HostIdentity: detect.HostIdentity, InstanceIdentity: detect.InstanceIdentity, Backend: LauncherRex,
		Profile: detect.Profile.Identity(), Binding: &created.Binding, Placement: created.Binding.Placement}
	journal.RootIdentity, err = canonicalIdentity(req.Root.Base())
	if err != nil {
		t.Fatal(err)
	}
	journal.RootPhysical, err = fsq.StableTreeIdentityInfo(req.Root.FileInfo())
	if err != nil {
		t.Fatal(err)
	}
	f.outside = true
	reclaimed, err := b.Reclaim(ReclaimRequest{Context: context.Background(), Journal: journal, Root: req.Root})
	if err != nil || reclaimed.Status != ReclaimAdoptable || !reflect.DeepEqual(reclaimed.Binding, created.Binding) {
		t.Fatalf("Reclaim = %#v, %v; want binding %#v", reclaimed, err, created.Binding)
	}
	closed, err := b.Close(CloseRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	inspected, err = b.Inspect(InspectRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || inspected.Status != InspectAbsent {
		t.Fatalf("Inspect after close = %#v, %v", inspected, err)
	}
	if len(f.sessions[0].Windows) != 1 || len(f.sessions[1].Windows) != 1 {
		t.Fatalf("close changed unrelated windows: %#v", f.sessions)
	}
}

func TestRexCreateSendsExactLineAfterHealthGate(t *testing.T) {
	b, f := newFakeRexBackend(t)
	f.loginPolls = 2
	req := rexCreateRequest(t)
	req.Plan.Agents[0].EnvOverlay = map[string]string{rexResumeTokenEnv: "inherited", "EXAMPLE": "two words"}
	original := cloneEnv(req.Plan.Agents[0].EnvOverlay)
	created, err := b.Create(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Plan.Agents[0].EnvOverlay, original) || req.Plan.Agents[1].EnvOverlay != nil {
		t.Fatalf("Create changed plan env: %#v", req.Plan.Agents)
	}
	req.AMQPath, err = filepath.EvalSymlinks(req.AMQPath)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := rexBoundBlocks(created.Binding)
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	for i, call := range f.calls {
		if call[0] != "send" {
			continue
		}
		agent := req.Plan.Agents[sent]
		agent.EnvOverlay = cloneEnv(agent.EnvOverlay)
		if agent.EnvOverlay == nil {
			agent.EnvOverlay = make(map[string]string)
		}
		agent.EnvOverlay[rexResumeTokenEnv] = ""
		want := agentCommand(req, agent)
		if call[6] != want || !strings.Contains(want, "OSC7577_TOKEN=''") || call[4] != "block:"+blocks[sent] {
			t.Fatalf("send = %v, want line %q", call, want)
		}
		if i+1 >= len(f.calls) || !reflect.DeepEqual(f.calls[i+1], []string{"send-key", "-s", f.sessions[0].ID, "-b", "block:" + blocks[sent], "enter"}) {
			t.Fatalf("send not followed by enter: %v", f.calls[i:])
		}
		if f.polls["block:"+blocks[sent]] != 3 {
			t.Fatalf("health polls = %v", f.polls)
		}
		sent++
	}
	if sent != 2 {
		t.Fatalf("sent %d lines, want 2", sent)
	}
}

func TestRexServerRestartIsDifferentBackendContext(t *testing.T) {
	b, f := newFakeRexBackend(t)
	req := rexCreateRequest(t)
	created, err := b.Create(req)
	if err != nil {
		t.Fatal(err)
	}
	f.instance = "019c5a10-75d8-7eef-8db7-ffffffffffff"
	if b.Detect().InstanceIdentity == created.Binding.InstanceIdentity {
		t.Fatal("server restart reused binding identity")
	}
	inspected, err := b.Inspect(InspectRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || inspected.Status != InspectUnknown || !inspected.ActionRequired || !strings.Contains(inspected.Evidence, "different backend context") {
		t.Fatalf("Inspect = %#v, %v", inspected, err)
	}
}

func TestRexListErrorIsUnknownNotAbsent(t *testing.T) {
	b, f := newFakeRexBackend(t)
	req := rexCreateRequest(t)
	created, err := b.Create(req)
	if err != nil {
		t.Fatal(err)
	}
	f.fail = "ls"
	inspected, err := b.Inspect(InspectRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || inspected.Status != InspectUnknown || !inspected.ActionRequired {
		t.Fatalf("Inspect = %#v, %v", inspected, err)
	}
	f.fail = ""
	inspected, err = b.Inspect(InspectRequest{Binding: created.Binding, Root: req.Root})
	if err != nil || inspected.Status != InspectPresent {
		t.Fatalf("Inspect after listing recovers = %#v, %v", inspected, err)
	}
}

func TestRexDetectUnavailableOutsidePane(t *testing.T) {
	b, f := newFakeRexBackend(t)
	f.outside = true
	detect := b.Detect()
	if detect.Available || len(detect.Degradations) != len(RexProfile().Capabilities) || detect.InstanceIdentity != rexInstancePrefix+f.instance || detect.HostIdentity != "host.test" {
		t.Fatalf("Detect = %#v", detect)
	}
	if err := detect.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRexPlacementRowsSplitsBelow(t *testing.T) {
	b, f := newFakeRexBackend(t)
	req := rexCreateRequest(t)
	req.Placement = &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutRows}
	created, err := b.Create(req)
	if err != nil {
		t.Fatal(err)
	}
	splits := 0
	for _, call := range f.calls {
		if call[0] == "split" {
			splits++
			if !slices.Contains(call, "--split=below") {
				t.Fatalf("rows split = %v", call)
			}
		}
	}
	if splits != 1 || created.Binding.Placement.Effective != *req.Placement {
		t.Fatalf("splits=%d placement=%#v", splits, created.Binding.Placement)
	}
}

func TestRexPlacementStaggersBetweenSplits(t *testing.T) {
	b, f := newFakeRexBackend(t)
	req := rexCreateRequest(t)
	req.Placement = &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns, StaggerMS: 250}
	var delays []time.Duration
	b.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		if last := f.calls[len(f.calls)-1]; last[0] != "block" || last[1] != "rename" {
			t.Fatalf("stagger not before split: %v", f.calls)
		}
		return nil
	}
	if _, err := b.Create(req); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delays, []time.Duration{250 * time.Millisecond}) {
		t.Fatalf("stagger sleeps = %v", delays)
	}
}

func TestRexCrashAfterWindowIsUncertain(t *testing.T) {
	for _, stage := range []string{"split", "send-key"} {
		t.Run(stage, func(t *testing.T) {
			b, f := newFakeRexBackend(t)
			f.fail = stage
			req := rexCreateRequest(t)
			_, err := b.Create(req)
			var definite *DefinitePreCreateError
			if !errors.Is(err, f.failure) || errors.As(err, &definite) {
				t.Fatalf("Create error = %v, want uncertain cause", err)
			}
			wantWindows := 1
			if stage == "split" {
				if !strings.Contains(err.Error(), "closed orphan rex window") {
					t.Fatalf("missing orphan reconciliation evidence: %v", err)
				}
			} else {
				wantWindows = 2
				if strings.Contains(err.Error(), "orphan") {
					t.Fatalf("reconciled window after input: %v", err)
				}
			}
			if len(f.sessions[0].Windows) != wantWindows {
				t.Fatalf("windows=%#v, want %d", f.sessions[0].Windows, wantWindows)
			}
		})
	}
}

func TestReconcileAutoSelectsRexWhenInsideRex(t *testing.T) {
	tmux := &reconcileBackend{name: LauncherTMux, inspect: InspectAbsent}
	rex := &reconcileBackend{name: LauncherRex, inspect: InspectAbsent}
	req := reconcileFixture(t, tmux)
	req.Launcher = LauncherAuto
	req.Preferences = []string{LauncherTMux, LauncherRex}
	req.Backends[LauncherRex] = rex
	t.Setenv("CMUX_SURFACE_ID", "")
	t.Setenv("TERM_PROGRAM", "rex")
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != LauncherRex || rex.creates != 1 || tmux.creates != 0 {
		t.Fatalf("result=%#v rex creates=%d tmux creates=%d", result, rex.creates, tmux.creates)
	}
}
