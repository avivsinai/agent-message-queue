package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"

	"github.com/avivsinai/agent-message-queue/internal/agentidentity"
	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Bead agent-message-queue-611.31: attach registers the invoking session in a
// running endpoint without a restart, and persists it for the next start.
func TestLiveRegisterAttachesAndPersistsTheTarget(t *testing.T) {
	root, err := os.MkdirTemp("", "ar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, stateDirName)
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	srv, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetRegistrar(liveRegistrar(root, stateDir, "remote", ep))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = ep.Close(); _ = store.Close() })

	session, err := registerTarget(stateDir, ipc.RegisterRequest{Kind: "fake", Target: "fake", Epoch: "e_1"})
	if err != nil || session.TargetID != "fake" {
		t.Fatalf("register: %+v %v", session, err)
	}
	if native, err := nativeSessionOf(stateDir, "fake"); err != nil || native == "" {
		t.Fatalf("native: %q %v", native, err)
	}
	f, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil || len(f.Adapters) != 1 || f.Adapters[0].Target != "fake" {
		t.Fatalf("manifest: %+v %v", f.Adapters, err)
	}
}

// Codex #885 P1 #2: registering a target name the manifest already gives to
// another session kept the other session silently.
func TestPersistAdapterRefusesAnotherSessionsTarget(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := persistAdapter(stateDir, manifest.Adapter{Kind: "fake", Target: "fake", Epoch: "e_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := persistAdapter(stateDir, manifest.Adapter{Kind: "fake", Target: "fake", Epoch: "e_2"}); err == nil {
		t.Fatal("a conflicting entry for the same target was accepted")
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Codex #895 P1 #4: mailbox attach accepted an AM_ROOT outside the pinned
// AM_BASE_ROOT/AM_SESSION.
func TestMailboxAttachRefusesAForeignRoot(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	foreign := filepath.Join(dir, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AM_ROOT", foreign)
	t.Setenv("AM_ME", "agent")
	t.Setenv("AM_BASE_ROOT", dir)
	t.Setenv("AM_SESSION", "authorized-session")
	if code, err := attach([]string{"--self"}, io.Discard, io.Discard); err == nil || code == 0 {
		t.Fatalf("a mismatched session pin was accepted: code=%d err=%v", code, err)
	}
}

// Codex #895 P2 #5: off failed outside AMQ because detach needed --root.
func TestDetachOutsideAMQUsesTheBoundRoot(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_ME", "")
	if err := binding.Write(binding.Binding{Root: dir, Target: "claude:7", NativeSession: "session-7"}); err != nil {
		t.Fatal(err)
	}
	// A fixed invoking session, not this machine's ambient one (codex #895
	// r2: the first version passed only inside a real Claude session).
	saved := selfIdentity
	t.Cleanup(func() { selfIdentity = saved })
	selfIdentity = func(root, _ string) (string, string, error) {
		if root != dir {
			t.Errorf("detach resolved self under %q; want the bound root %q", root, dir)
		}
		return "claude:7", "session-7", nil
	}
	if code, err := detach([]string{"--self"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("detach outside AMQ: code=%d err=%v", code, err)
	}
	if _, err := binding.Read(); !errors.Is(err, binding.ErrNone) {
		t.Fatalf("binding still present after detach: %v", err)
	}
}

// Bead agent-message-queue-94w (review F13): detach with no scope removed
// every binding; it needs --self, --name or --all.
func TestDetachNeedsAScope(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	for _, n := range []string{"one", "two", "three"} {
		b := binding.Binding{Carrier: binding.CarrierMailbox, Root: dir, Handle: n, Name: n}
		if err := binding.WriteNamed(b); err != nil {
			t.Fatal(err)
		}
	}
	if code, err := detach(nil, io.Discard, io.Discard); code != protocol.ExitUsage || err == nil {
		t.Fatalf("detach with no scope: code=%d err=%v; want usage error", code, err)
	}
	if all, _ := binding.List(); len(all) != 3 {
		t.Fatalf("%d bindings after a refused detach; want 3", len(all))
	}
	if code, err := detach([]string{"--name", "two"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("detach --name: code=%d err=%v", code, err)
	}
	if all, _ := binding.List(); len(all) != 2 {
		t.Fatalf("%d bindings after detach --name; want 2", len(all))
	}
	if code, err := detach([]string{"--all"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("detach --all: code=%d err=%v", code, err)
	}
	if all, _ := binding.List(); len(all) != 0 {
		t.Fatalf("%d bindings after detach --all; want 0", len(all))
	}
}

// Bead agent-message-queue-za4 (review F12): a reply to a Buzz DM warned
// "may not be read" because attach never listed buzz in the root's roster.
func TestAttachListsBuzzInTheRosterOnce(t *testing.T) {
	root := canonicalTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "meta"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "meta", "config.json")
	if err := os.WriteFile(cfg, []byte(`{"version":1,"created_utc":"2026-01-01T00:00:00Z","agents":["claude"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := listBuzzInRoster(root); err != nil {
			t.Fatal(err)
		}
	}
	got, err := config.LoadConfig(cfg)
	if err != nil || len(got.Agents) != 2 || got.Agents[1] != "buzz" {
		t.Fatalf("agents = %v err=%v; want [claude buzz]", got.Agents, err)
	}
	bare := canonicalTempDir(t)
	if err := listBuzzInRoster(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare, "meta", "config.json")); err == nil {
		t.Fatal("attach created a config.json in a root that had none")
	}
}

// Bead agent-message-queue-5re (review F13): attach and detach parsed --json
// and ignored it.
func TestDetachAllJSONPrintsTheRemovedNames(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: dir, Handle: "one", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"detach", "--all", "--json"}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var got struct {
		Removed []string `json:"removed"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Removed) != 1 || got.Removed[0] != "one" {
		t.Fatalf("output %q (%v); want JSON removed [one]", out.String(), err)
	}
}

// Pro review of 94w: a whitespace-only native --name picked the default name
// but counted as explicit, so it replaced another session's binding.
func TestNativeBlankNameIsNotExplicit(t *testing.T) {
	name, explicit := nativeBindingName("  ", "claude:7")
	if explicit || name == "" {
		t.Fatalf("name=%q explicit=%v; want the default name, not explicit", name, explicit)
	}
}

// Pro review of 5re: the error path scanned for the token while the success
// path used the parsed value, so --json=false gave a JSON error but human
// success, and -json=true gave a human error.
func TestJSONFlagValueDecidesSuccessAndErrorOutput(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	var out, errOut bytes.Buffer
	if code := run([]string{"detach", "-json=true"}, nil, &out, &errOut); code != protocol.ExitUsage {
		t.Fatalf("exit %d; want usage", code)
	}
	var body map[string]any
	if err := json.Unmarshal(out.Bytes(), &body); err != nil || body["error"] == nil {
		t.Fatalf("-json=true error output %q (%v); want a JSON error on stdout", out.String(), err)
	}
	out.Reset()
	if code := run([]string{"detach", "--all", "--json=false"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if json.Valid(out.Bytes()) {
		t.Fatalf("--json=false printed JSON: %q", out.String())
	}
	out.Reset()
	_ = run([]string{"detach", "--json=false"}, nil, &out, &errOut)
	if out.Len() != 0 {
		t.Fatalf("--json=false error went to stdout as %q; want human text on stderr", out.String())
	}
	// Pro review of 5re r2: a string option's value is never a flag, so a
	// value of "--" or "--json=false" must not change the JSON decision.
	for _, args := range [][]string{
		{"detach", "--name", "--", "--json", "--all"},
		{"submit", "--root", dir, "--text", "--", "--json", "T"},
		{"submit", "--root", dir, "--json", "--text", "--json=false", "T"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(args, nil, &out, &errOut); code == 0 {
			t.Fatalf("%v succeeded", args)
		}
		var body map[string]any
		if err := json.Unmarshal(out.Bytes(), &body); err != nil || body["error"] == nil {
			t.Fatalf("%v printed %q (%v); want a JSON error on stdout", args, out.String(), err)
		}
	}
}

// E2E kit F3: after a session change in the same Claude process (same target,
// new native session), attach --self --link refused the binding's own name,
// so a link share could not follow it. With --link the same root and target
// re-pin the name, and another target still may not take it. A plain native
// attach of another session takes the next free name instead of the --name
// refusal of ruling ww: Buzz DX ruling 2 gives each session its own name.
func TestWriteBindingRepinsTheSameTargetAfterASessionChange(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	old := binding.Binding{Root: dir, Target: "claude:7", NativeSession: "session-old", Name: "claude-7"}
	if _, _, err := writeBinding(old, false, true, binding.Binding.Liveness); err != nil {
		t.Fatal(err)
	}
	moved := old
	moved.NativeSession = "session-new"
	if name, _, err := writeBinding(moved, false, false, binding.Binding.Liveness); err != nil || name != "claude-7-2" {
		t.Fatalf("native attach without --link: name=%q err=%v; want claude-7-2", name, err)
	}
	if _, _, err := writeBinding(moved, false, true, binding.Binding.Liveness); err != nil {
		t.Fatalf("re-attach with --link after a session change: %v", err)
	}
	if got, err := binding.ReadNamed("claude-7"); err != nil || got.NativeSession != "session-new" {
		t.Fatalf("binding = %+v %v; want it pinned to session-new", got, err)
	}
	other := binding.Binding{Root: dir, Target: "claude:8", NativeSession: "session-8", Name: "claude-7"}
	var taken *binding.NameTakenError
	if _, _, err := writeBinding(other, false, true, binding.Binding.Liveness); !errors.As(err, &taken) {
		t.Fatalf("another target with the same name: err=%v; want NameTakenError", err)
	}
}

// Observed 2026-10-15: attach --self in session3 refused `binding name
// "claude-agent-message-queue" already names another session`, because the
// binding of session1 of the same project held the default <handle>-<project>
// name. Each session now gets its own name: a different session never takes
// or refuses on another session's name, and the same session keeps the name
// it already holds.
func TestMailboxAttachNamesEachSessionOfAProject(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root1 := filepath.Join(dir, "p", ".agent-mail", "session1")
	root3 := filepath.Join(dir, "p", ".agent-mail", "session3")
	other := filepath.Join(dir, "q", "p", ".agent-mail", "session3")
	for _, r := range []string{root1, root3, other} {
		if err := os.MkdirAll(r, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := binding.Binding{Carrier: binding.CarrierMailbox, Root: root1, Handle: "claude", Name: "claude-p"}
	if err := binding.WriteNamed(legacy); err != nil {
		t.Fatal(err)
	}
	if got := attachMailboxAs(t, root3); got != "claude-p-session3" {
		t.Fatalf("session3 attached as %q; want claude-p-session3", got)
	}
	if got := attachMailboxAs(t, other); got != "claude-p-session3-2" {
		t.Fatalf("another project's session3 attached as %q; want claude-p-session3-2", got)
	}
	if got := attachMailboxAs(t, root1); got != "claude-p" {
		t.Fatalf("session1 attached as %q; want its own name claude-p", got)
	}
	all, err := binding.List()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"claude-p": root1, "claude-p-session3": root3, "claude-p-session3-2": other}
	if len(all) != len(want) {
		t.Fatalf("bindings %+v; want %v", all, want)
	}
	for _, b := range all {
		if want[b.Name] != b.Root {
			t.Fatalf("binding %s names %s; want %s", b.Name, b.Root, want[b.Name])
		}
	}
}

// Buzz DX S2: /amq-remote status lists the bound sessions. A session proven
// ended (its holder ran in another boot) is pruned; one with no holder is
// unknown, listed, and kept.
func TestBindingsPrunesEndedSessionsAndKeepsUnknownOnes(t *testing.T) {
	self := endedHolder(t)
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	for _, b := range []binding.Binding{
		{Carrier: binding.CarrierMailbox, Root: dir, Handle: "codex", Name: "ended", Holder: self},
		{Carrier: binding.CarrierMailbox, Root: dir, Handle: "claude", Name: "unknown"},
	} {
		if err := binding.WriteNamed(b); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code := run([]string{"bindings", "--json"}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("bindings exit %d: %s", code, out.String())
	}
	var got struct {
		Bindings []struct{ Name, State string }
		Pruned   []string
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("bindings output %q: %v", out.String(), err)
	}
	if len(got.Pruned) != 1 || got.Pruned[0] != "ended" || len(got.Bindings) != 1 ||
		got.Bindings[0].Name != "unknown" || got.Bindings[0].State != string(binding.Unknown) {
		t.Fatalf("bindings output %s; want ended pruned and unknown listed", out.String())
	}
	if _, err := binding.ReadNamed("ended"); !errors.Is(err, binding.ErrNone) {
		t.Fatalf("the ended binding is still present: %v", err)
	}
}

// bindings --json shows each session's unresolved dialogs, so the owner or
// an agent can see one that is stuck.
func TestBindingsShowsOpenDialogs(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: dir, Handle: "claude", Name: "proj"}); err != nil {
		t.Fatal(err)
	}
	dialogs, err := dialog.DirFor("proj")
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", Text: "$ ls", OpenedAt: opened,
		Options: []dialog.Option{{N: 1, Label: "Allow once", Value: "allow"}}}
	if err := dialog.Open(dialogs, req); err != nil {
		t.Fatal(err)
	}
	if _, err := dialog.WriteAnswer(dialogs, dialog.Answer{ID: req.ID, Option: 1, By: dialog.ByBuzz}); err != nil {
		t.Fatal(err)
	}
	posts := filepath.Join(dir, "remote", "acp", "dialogs", "proj")
	if err := os.MkdirAll(posts, 0o700); err != nil {
		t.Fatal(err)
	}
	post := `{"channel":"c","event":"` + strings.Repeat("a", 64) + `","text":"$ ls"}`
	if err := os.WriteFile(filepath.Join(posts, req.ID+".json"), []byte(post), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"bindings", "--json"}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("bindings exit %d: %s", code, out.String())
	}
	var got struct {
		Bindings []struct {
			Name    string
			Dialogs []struct {
				ID, Tool, Phase string
				Posted          bool
				OpenedAt        time.Time `json:"opened_at"`
			}
		}
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("bindings output %q: %v", out.String(), err)
	}
	if len(got.Bindings) != 1 || len(got.Bindings[0].Dialogs) != 1 {
		t.Fatalf("bindings output %s; want one session with one dialog", out.String())
	}
	d := got.Bindings[0].Dialogs[0]
	if d.ID != req.ID || d.Tool != "Bash" || d.Phase != string(dialog.PhaseAnswered) || !d.Posted || !d.OpenedAt.Equal(opened) {
		t.Fatalf("dialog row %+v; want the answered, posted Bash dialog", d)
	}
}

// attachMailboxAs runs attach --self --json as the AMQ handle claude of the
// session at root, pinned as a real session is, and returns the bound name.
func attachMailboxAs(t *testing.T, root string) string {
	t.Helper()
	pinMailboxSession(t, root)
	var out bytes.Buffer
	if code, err := attach([]string{"--self", "--json"}, &out, io.Discard); code != 0 || err != nil {
		t.Fatalf("attach from %s: code=%d err=%v", root, code, err)
	}
	var got struct{ Name string }
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("attach output %q: %v", out.String(), err)
	}
	return got.Name
}

// pinMailboxSession sets the environment of the AMQ handle claude of the
// session at root, pinned as a real session is.
func pinMailboxSession(t *testing.T, root string) {
	t.Helper()
	rootID, err := fsq.StableTreeIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	baseID, err := fsq.StableTreeIdentity(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"AM_ROOT": root, "AM_ME": "claude", "AM_BASE_ROOT": filepath.Dir(root),
		"AM_SESSION": filepath.Base(root), "AM_ROOT_ID": rootID, "AM_BASE_ROOT_ID": baseID} {
		t.Setenv(k, v)
	}
}

// runApprovalHook runs count approval hooks at once in this process, each
// for one Bash dialog of session, with the binding writer the installed hook
// uses, and returns the dialogs open in name's directory once count are, or
// after 1 s. The hooks then end as answered in the terminal.
func runApprovalHook(t *testing.T, count int, session, name string) []dialog.Request {
	t.Helper()
	done, ended := make(chan struct{}), make(chan int, count)
	for range count {
		go func() {
			ended <- claude.RunPermissionHook(strings.NewReader(`{"session_id":"`+session+`","tool_name":"Bash","tool_input":{"command":"make"}}`),
				io.Discard, done, hookBinding)
		}()
	}
	ddir, err := dialog.DirFor(name)
	if err != nil {
		t.Fatal(err)
	}
	var open []dialog.Request
	for deadline := time.Now().Add(time.Second); len(open) < count && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		open, _ = dialog.Requests(ddir)
	}
	close(done)
	for range count {
		<-ended
	}
	return open
}

// Two Claude seats in one AMQ session get labels, and so Buzz channels, that
// name their handles.
//
// Observed 2026-11-07: seats claude and claude2 in one session both got a
// channel labelled "claude · <project>/<session> @ <machine>".
func TestMailboxSeatsInOneSessionAreLabelledByHandle(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root := filepath.Join(dir, "p", ".agent-mail", "session1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	saved := detectHarness
	t.Cleanup(func() { detectHarness = saved })
	detectHarness = func(*codexView, string, string) harnessSession { return harnessSession{Kind: "claude"} }
	pinMailboxSession(t, root)
	labels := map[string]string{}
	for _, handle := range []string{"claude", "claude2"} {
		t.Setenv("AM_ME", handle)
		var out bytes.Buffer
		if code, err := attach([]string{"--self", "--json"}, &out, io.Discard); code != 0 || err != nil {
			t.Fatalf("attach as %s: code=%d err=%v", handle, code, err)
		}
		var got struct{ Name string }
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("attach output %q: %v", out.String(), err)
		}
		b, err := binding.ReadNamed(got.Name)
		if err != nil {
			t.Fatal(err)
		}
		labels[handle] = b.Label
	}
	if labels["claude"] != binding.SessionLabel("claude", "p/session1", binding.Machine()) ||
		labels["claude2"] != binding.SessionLabel("claude2", "p/session1", binding.Machine()) {
		t.Fatalf("labels = %q; want each seat named by its handle", labels)
	}
}

// An AMQ session that never ran /amq-remote gets its mailbox binding from
// its first approval dialog, as attach --self writes it, and the dialog
// opens there; a later attach --self keeps that one binding.
func TestApprovalHookBindsAnAMQSessionsMailbox(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root := filepath.Join(dir, "p", ".agent-mail", "session1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	pinMailboxSession(t, root)
	if open := runApprovalHook(t, 1, "s-1", "claude-p-session1"); len(open) != 1 || open[0].Text != "$ make" {
		t.Fatalf("open dialogs = %+v; want the dialog in the mailbox binding's directory", open)
	}
	b, err := binding.ReadNamed("claude-p-session1")
	if err != nil || !b.Mailbox() || b.Root != root || b.Handle != "claude" || b.Label != binding.SessionLabel("claude", "p/session1", binding.Machine()) {
		t.Fatalf("binding = %+v, %v; want the mailbox binding attach --self writes", b, err)
	}
	if got := attachMailboxAs(t, root); got != "claude-p-session1" {
		t.Fatalf("attach --self wrote %q; want the hook's binding", got)
	}
	if all, _ := binding.List(); len(all) != 1 {
		t.Fatalf("bindings = %+v; want one", all)
	}
}

// After /amq-remote off the approval hook writes no binding for the
// session again: its dialogs stay in the terminal until the owner connects
// it, also when the same session resumes in another process, and for any
// session in the process that was turned off. Another session in another
// process is not off.
//
// Regression: code review of the hook-written binding found that the first
// dialog after /amq-remote off connected the session again.
// Regressions (review of the resumed-session reattach): the hook keyed a
// detach on the process only, so a resumed session connected again; then
// on the session only, so /clear, or a hook binding that names no session,
// in the process that was turned off connected again.
func TestApprovalHookLeavesADetachedSessionOff(t *testing.T) {
	this, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	ancestors := launch.Ancestors()
	if len(ancestors) == 0 {
		t.Skip("no process ancestry on this platform")
	}
	other := ancestors[0] // a live process that is not this one
	saved := detectHarness
	t.Cleanup(func() { detectHarness = saved })
	var holder launch.HolderIdentity
	transcript := ""
	detectHarness = func(*codexView, string, string) harnessSession {
		return harnessSession{Kind: "claude", PID: holder.PID, Holder: holder, Transcript: transcript}
	}
	for _, tc := range []struct {
		name     string
		detached launch.HolderIdentity
		off, now string // the transcript when turned off, and at the dialog
		want     int
	}{
		{"this process", this, "", "", 0},
		{"another process", other, "", "", 1},
		{"this session resumed in another process", other, "/projects/p/s-1.jsonl", "/projects/p/s-1.jsonl", 0},
		{"this process after /clear", this, "/projects/p/s-1.jsonl", "/projects/p/s-2.jsonl", 0},
		{"this process, no session at the dialog", this, "/projects/p/s-1.jsonl", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := canonicalTempDir(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
			root := filepath.Join(dir, "p", ".agent-mail", "session1")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			holder, transcript = tc.detached, tc.off
			attachMailboxAs(t, root)
			if code, err := detach([]string{"--self"}, io.Discard, io.Discard); code != 0 || err != nil {
				t.Fatalf("detach: code=%d err=%v", code, err)
			}
			holder, transcript = this, tc.now
			open := runApprovalHook(t, 1, "s-1", "claude-p-session1")
			if len(open) != tc.want {
				t.Fatalf("open dialogs = %d; want %d", len(open), tc.want)
			}
			if all, _ := binding.List(); len(all) != tc.want {
				t.Fatalf("bindings = %+v; want %d", all, tc.want)
			}
		})
	}
}

// /amq-remote off in one session turns off only that session: an earlier
// session on the same handle that ended and was pruned keeps its ended
// tombstone, so a resume of that session still comes back.
//
// Regression (review of the off-after-prune fix): a mailbox --self detach
// turned every ended tombstone of the handle into a detached one.
func TestDetachSelfLeavesAnotherSessionsEndedTombstone(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root := filepath.Join(dir, "p", ".agent-mail", "session1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	saved := detectHarness
	t.Cleanup(func() { detectHarness = saved })
	session := "s-1"
	detectHarness = func(*codexView, string, string) harnessSession { return harnessSession{Kind: "pi", Session: session} }
	name := attachMailboxAs(t, root)
	if _, err := binding.Retire(binding.RemovedEnded, func(b binding.Binding) bool { return b.Name == name }); err != nil {
		t.Fatal(err)
	}
	session = "s-2"
	var out bytes.Buffer
	if code, err := detach([]string{"--self"}, &out, io.Discard); code != 0 || err != nil {
		t.Fatalf("detach: code=%d err=%v", code, err)
	}
	tombs, err := binding.Tombstones()
	if err != nil || len(tombs) != 1 || tombs[0].Removed != binding.RemovedEnded || strings.TrimSpace(out.String()) != "Not connected." {
		t.Fatalf("tombstones = %+v, %v; output %q; want s-1 still ended", tombs, err, out.String())
	}
}

// A Claude session outside AMQ that no binding serves (the desktop app, a
// plain terminal) gets the native binding attach --self --native writes,
// once for two dialogs at a time, and its dialogs open there.
func TestApprovalHookBindsANativeSession(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_ME", "")
	ancestors := launch.Ancestors()
	if len(ancestors) == 0 {
		t.Skip("no process ancestry on this platform")
	}
	pid := ancestors[0].PID
	config := canonicalTempDir(t)
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	// No messaging socket and no kind: the hook never submits to the
	// session, so any registry entry will do.
	raw, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "s-desktop", "cwd": "/work/myproj"})
	if err := os.MkdirAll(filepath.Join(config, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "sessions", strconv.Itoa(pid)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	saved := claudeSession
	t.Cleanup(func() { claudeSession = saved })
	claudeSession = func(int) (claude.Session, error) { return claude.Session{ID: "s-desktop", Cwd: "/work/myproj"}, nil }
	if open := runApprovalHook(t, 2, "s-desktop", "claude-myproj"); len(open) != 2 {
		t.Fatalf("open dialogs = %+v; want both in the native binding's directory", open)
	}
	root, err := binding.MachineRoot()
	if err != nil {
		t.Fatal(err)
	}
	holder, err := launch.HolderOf(pid)
	if err != nil {
		t.Fatal(err)
	}
	b, err := binding.ReadNamed("claude-myproj")
	if err != nil || b.Mailbox() || b.Root != root || b.Target != "claude:"+strconv.Itoa(pid) || b.NativeSession != "s-desktop" ||
		b.Harness != "claude" || b.Holder != holder || b.Label != binding.SessionLabel("claude", "myproj", binding.Machine()) ||
		b.Identity != binding.NativeIdentity("claude", root, "/work/myproj") {
		t.Fatalf("binding = %+v, %v; want the native binding attach --self --native writes", b, err)
	}
	if all, _ := binding.List(); len(all) != 1 {
		t.Fatalf("bindings = %+v; want one", all)
	}
}

// Buzz DX S5: a session whose Buzz agent runs is told only whom to DM: two
// lines, no Buzz setup step.
func TestAttachWithARunningAgentNamesOnlyTheDM(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root := filepath.Join(dir, "p", ".agent-mail", "session3")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	markers := filepath.Join(dir, "remote", "acp", "agents", "claude-p-session3")
	if err := os.MkdirAll(markers, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := `{"name":"claude-p-session3","pubkey":"ab","display_name":"AMQ: claude-p-session3","updated":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(markers, "ab.json"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	pinMailboxSession(t, root)
	var out bytes.Buffer
	if code, err := attach([]string{"--self"}, &out, io.Discard); code != 0 || err != nil {
		t.Fatalf("attach: code=%d err=%v", code, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "Connected: ") || lines[1] != `DM "AMQ: claude-p-session3" in Buzz.` {
		t.Fatalf("attach printed %q; want Connected and the DM line only", out.String())
	}
}

// Review-s5 P1-1: attach promised Desktop's create dialog whenever the
// helper ran, but the helper drafts only for a session proven live. A
// session whose liveness is unknown never got the dialog, and nothing told
// the owner to create the agent. It is now told to.
func TestAttachOffersSetupWhenTheHelperWillNotDraft(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root := filepath.Join(dir, "p", ".agent-mail", "session3")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "remote", "acp", "agents", helperMarkerDir)
	if err := os.MkdirAll(helper, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := `{"name":"_helper","pubkey":"hh","channel":"c1","owner_admin":true,"updated":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(helper, "hh.json"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	pinMailboxSession(t, root)
	var out bytes.Buffer
	if code, err := attach([]string{"--self"}, &out, io.Discard); code != 0 || err != nil {
		t.Fatalf("attach: code=%d err=%v", code, err)
	}
	if !strings.Contains(out.String(), "amq-acp setup --machine") || strings.Contains(out.String(), "Create agent") {
		t.Fatalf("attach printed %q; want the machine setup step, not the create dialog, for a session whose liveness is unknown", out.String())
	}
}

// field: a note on bead agent-message-queue-1uk: after the helper agent was
// deleted in Buzz Desktop, `amq-remote bindings` showed "Helper: ...
// (offline)" from its last marker for ever, beside the machine agent that
// serves every session.
func TestBindingsHidesAnOfflineHelperBesideTheMachineAgent(t *testing.T) {
	gone := endedHolder(t)
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	now := time.Now().UTC()
	for _, m := range []struct {
		dir     string
		updated time.Time
		holder  launch.HolderIdentity
	}{
		{helperMarkerDir, now.Add(-time.Hour), gone},
		{machineMarkerDir, now, self},
	} {
		path := filepath.Join(dir, "remote", "acp", "agents", m.dir)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(map[string]any{"name": m.dir, "updated": m.updated.Format(time.RFC3339), "worker": m.holder, "parent": m.holder})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "pk.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code, err := bindings(nil, &out); code != 0 || err != nil {
		t.Fatalf("bindings: code=%d err=%v", code, err)
	}
	if !strings.Contains(out.String(), "Machine agent:") || strings.Contains(out.String(), "Helper:") {
		t.Fatalf("bindings printed %q; want the machine agent and no line for the offline helper", out.String())
	}
}

// endedHolder is this test process as a holder recorded in another boot, so
// its binding is proven ended; the test skips where the boot id is unreadable.
func endedHolder(t *testing.T) launch.HolderIdentity {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process start times and boot ids are read on darwin and linux only")
	}
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if self.BootID == "" {
		t.Skip("boot id unavailable")
	}
	// Another boot in the form the platform reports (a UUID): a different
	// boot proves the holder ended only when both ids are UUIDs.
	self.BootID = "00000000-0000-4000-8000-000000000001"
	return self
}

// Review-s2 P1-1: pruning an ended binding freed its name, so the next
// session of another identity that computed the same name took over the
// ended session's Buzz agent and DM. The name stays held by its identity.
func TestPrunedNameStaysWithItsSession(t *testing.T) {
	ended := endedHolder(t)
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	mine := filepath.Join(dir, "p", ".agent-mail", "session3")
	theirs := filepath.Join(dir, "q", "p", ".agent-mail", "session3")
	for _, r := range []string{mine, theirs} {
		if err := os.MkdirAll(r, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := binding.Binding{Carrier: binding.CarrierMailbox, Root: theirs, Handle: "claude", Name: "claude-p-session3", Holder: ended}
	if err := binding.WriteNamed(old); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"bindings"}, nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("bindings exit %d", code)
	}
	if got := attachMailboxAs(t, mine); got != "claude-p-session3-2" {
		t.Fatalf("another session attached as %q after the prune; want claude-p-session3-2", got)
	}
	if got := attachMailboxAs(t, theirs); got != "claude-p-session3" {
		t.Fatalf("the ended session came back as %q; want its own claude-p-session3", got)
	}
}

// Issue #1070: a Codex binding records no session process, so its liveness
// was always unknown and it was never pruned. Its thread now proves it: a
// thread the app-server that had it loaded no longer has loaded has ended.
func TestBindingsPrunesACodexThreadTheAppServerUnloaded(t *testing.T) {
	pruned, states := codexBindingsAfter(t, os.Getpid())
	if len(pruned) != 1 || pruned[0] != "codex-t-gone" || len(states) != 1 || states["codex-t-live"] != string(binding.Live) {
		t.Fatalf("pruned %v, listed %v; want codex-t-gone pruned and codex-t-live live", pruned, states)
	}
}

// Review-1070 P1-B2: a restarted app-server has no thread loaded, and
// "not loaded" was read as ended, so every live Codex session on that socket
// was pruned and its agent archived. Only the daemon process the thread was
// bound in proves it ended; another one leaves it unknown and kept.
func TestBindingsKeepsACodexThreadAfterTheAppServerRestarted(t *testing.T) {
	pruned, states := codexBindingsAfter(t, os.Getpid()+1)
	if len(pruned) != 0 || states["codex-t-gone"] != string(binding.Unknown) {
		t.Fatalf("pruned %v, listed %v; want codex-t-gone kept as unknown", pruned, states)
	}
}

// Review-stack P2-B5: a Codex update restarts the daemon, and the thread
// resumes in the new one. The binding kept the old daemon, so the new
// daemon's "not loaded" never proved the session ended, and the next session
// there got <name>-2 and a new agent. While the thread is loaded, the binding
// now follows the daemon that has it.
func TestCodexBindingFollowsARestartedAppServer(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process start times are read on darwin and linux only")
	}
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	savedLoaded, savedPID := codexLoadedThreads, codexDaemonPID
	t.Cleanup(func() { codexLoadedThreads, codexDaemonPID = savedLoaded, savedPID })
	codexDaemonPID = func(string) (int, error) { return os.Getpid(), nil }
	old := launch.HolderIdentity{PID: os.Getpid() + 1, ProcessStart: "1"}
	b := binding.Binding{Root: dir, Target: "codex:t1", NativeSession: "t1", Name: "codex-t1",
		AppServer: binding.AppServerThread{Socket: filepath.Join(dir, "app-server.sock"), Thread: "t1", Daemon: old}}
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	for _, loaded := range [][]string{{"t1"}, nil} {
		codexLoadedThreads = func(string) ([]string, error) { return loaded, nil }
		if code := run([]string{"bindings"}, nil, io.Discard, io.Discard); code != 0 {
			t.Fatalf("bindings exit %d", code)
		}
	}
	if _, err := binding.ReadNamed("codex-t1"); !errors.Is(err, binding.ErrNone) {
		t.Fatalf("the binding outlived its thread in the restarted app-server: %v", err)
	}
}

// codexBindingsAfter binds threads t-live and t-gone in an app-server whose
// process is this test, runs bindings with t-live loaded and the socket
// served by daemonPID, and returns the pruned names and each listed state.
func codexBindingsAfter(t *testing.T, daemonPID int) ([]string, map[string]string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process start times are read on darwin and linux only")
	}
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	daemon, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	savedLoaded, savedPID := codexLoadedThreads, codexDaemonPID
	t.Cleanup(func() { codexLoadedThreads, codexDaemonPID = savedLoaded, savedPID })
	codexLoadedThreads = func(string) ([]string, error) { return []string{"t-live"}, nil }
	codexDaemonPID = func(string) (int, error) { return daemonPID, nil }
	for _, thread := range []string{"t-live", "t-gone"} {
		b := binding.Binding{Root: dir, Target: "codex:" + thread, NativeSession: thread, Name: "codex-" + thread,
			AppServer: binding.AppServerThread{Socket: filepath.Join(dir, "app-server.sock"), Thread: thread, Daemon: daemon}}
		if err := binding.WriteNamed(b); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code := run([]string{"bindings", "--json"}, nil, &out, io.Discard); code != 0 {
		t.Fatalf("bindings exit %d: %s", code, out.String())
	}
	var got struct {
		Bindings []struct{ Name, State string }
		Pruned   []string
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("bindings output %q: %v", out.String(), err)
	}
	states := map[string]string{}
	for _, b := range got.Bindings {
		states[b.Name] = b.State
	}
	return got.Pruned, states
}

// A linked server's first live run (amq-remote 0.97.0): attach --self --link
// in a Claude Code session without the AMQ Stop hook left the run running
// forever, so every later task was parked busy. attach now refuses and names
// the command that installs the hook.
func TestAttachRefusesAClaudeSessionWithoutTheStopHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedFind, savedHook := findSelf, claudeStopHookState
	t.Cleanup(func() { findSelf, claudeStopHookState = savedFind, savedHook })
	findSelf = func(string, string) (registry.Candidate, error) {
		return registry.Candidate{Kind: "claude", Target: "claude:7", Config: json.RawMessage(`{"pid":7}`)}, nil
	}
	claudeStopHookState = func() (string, error) { return claude.HookMissing, nil }
	code, err := attach([]string{"--self", "--link", "srv"}, io.Discard, io.Discard)
	if code != protocol.ExitActionRequired || err == nil || !strings.Contains(err.Error(), "amq-remote claude install-stop-hook") {
		t.Fatalf("attach --link without the Stop hook: code=%d err=%v; want exit 6 naming amq-remote claude install-stop-hook", code, err)
	}
}

// Review-s2 P2-2: a Codex or pi session's identity used the working
// directory of the tool call attach ran in, so a call from a subdirectory
// gave the same session another name and Buzz agent. The identity uses the
// session's own directory: the Codex thread's from its app-server, the pi
// session's from its process.
func TestNativeHarnessUsesTheSessionsOwnCwd(t *testing.T) {
	savedThread, savedProc := codexThreadCwd, processCwd
	t.Cleanup(func() { codexThreadCwd, processCwd = savedThread, savedProc })
	codexThreadCwd = func(string, string) (string, string, error) { return "/work/codex-proj", "", nil }
	processCwd = func(int) (string, error) { return "/work/pi-proj", nil }
	for _, row := range []struct {
		cand registry.Candidate
		want string
	}{
		{registry.Candidate{Kind: "codex", Target: "codex:t1", Config: json.RawMessage(`{"socket":"/s.sock","thread":"t1"}`)}, "/work/codex-proj"},
		{registry.Candidate{Kind: "pi", Target: "pi:chat", PID: 7}, "/work/pi-proj"},
	} {
		if got := nativeHarness(newCodexView(), row.cand).Cwd; got != row.want {
			t.Fatalf("%s session cwd = %q; want its own %q", row.cand.Kind, got, row.want)
		}
	}
}

// Review-1070 P1-B1: a Codex attach recorded the app-server thread without
// proof, so a session on another app-server read as ended and was pruned
// right after attach. The thread is recorded only when the daemon has it
// loaded.
func TestNativeHarnessRecordsOnlyALoadedCodexThread(t *testing.T) {
	savedLoaded, savedPID, savedHolder := codexLoadedThreads, codexDaemonPID, processHolder
	t.Cleanup(func() { codexLoadedThreads, codexDaemonPID, processHolder = savedLoaded, savedPID, savedHolder })
	codexDaemonPID = func(string) (int, error) { return 42, nil }
	processHolder = func(pid int) (launch.HolderIdentity, error) {
		return launch.HolderIdentity{PID: pid, ProcessStart: "1"}, nil
	}
	cand := registry.Candidate{Kind: "codex", Target: "codex:t1", Config: json.RawMessage(`{"socket":"/s.sock","thread":"t1"}`)}
	for _, row := range []struct {
		loaded []string
		want   string
	}{{[]string{"t0"}, ""}, {[]string{"t0", "t1"}, "t1"}} {
		codexLoadedThreads = func(string) ([]string, error) { return row.loaded, nil }
		if got := nativeHarness(newCodexView(), cand).AppServer; got.Thread != row.want || row.want != "" && got.Daemon.PID != 42 {
			t.Fatalf("loaded %v: recorded %+v; want thread %q with daemon 42", row.loaded, got, row.want)
		}
	}
}

// Regression: agent-message-queue-1uk G2, mailbox attach from a Codex TUI
// recorded neither a daemon thread nor a holder, so Buzz made no channel.
// A loaded daemon thread stays first; an embedded TUI uses its ancestor.
func TestDetectHarnessFindsCodexSession(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process ancestry is unavailable on this platform")
	}
	parent, err := launch.HolderOf(os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_THREAD_ID", "this-thread")
	savedAncestors, savedExecutable, savedArgs, savedHolder := ancestorPIDs, processExecutable, processArgs, processHolder
	savedSocket, savedPID, savedLoaded, savedCwd := codexDefaultSocket, codexDaemonPID, codexLoadedThreads, codexThreadCwd
	t.Cleanup(func() {
		ancestorPIDs, processExecutable, processArgs, processHolder = savedAncestors, savedExecutable, savedArgs, savedHolder
		codexDefaultSocket, codexDaemonPID, codexLoadedThreads, codexThreadCwd = savedSocket, savedPID, savedLoaded, savedCwd
	})
	codexDefaultSocket = func() (string, error) { return "/daemon.sock", nil }
	codexDaemonPID = func(string) (int, error) { return 42, nil }
	processHolder = func(pid int) (launch.HolderIdentity, error) {
		if pid == parent.PID {
			return parent, nil
		}
		return launch.HolderIdentity{PID: pid, ProcessStart: "daemon"}, nil
	}
	codexThreadCwd = func(string, string) (string, string, error) { return "/project", "", nil }
	for _, row := range []struct {
		name   string
		loaded []string
		args   []string
		daemon bool
		holder bool
	}{
		{"shared daemon", []string{"this-thread"}, nil, true, false},
		{"embedded TUI", []string{"another-thread"}, []string{"codex", "-c", "model=local"}, false, true},
		// Review of 1uk G2: another app-server (observed PID 4664) can
		// be an ancestor, but its lifetime does not identify one thread.
		{"another daemon", []string{"another-thread"}, []string{"codex", "app-server", "--listen", "unix://"}, false, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			codexLoadedThreads = func(string) ([]string, error) { return row.loaded, nil }
			ancestorPIDs = func(pid int) ([]int, error) {
				if row.daemon || pid != os.Getpid() {
					t.Fatalf("ancestor lookup for pid %d with daemon=%v", pid, row.daemon)
				}
				return []int{parent.PID}, nil
			}
			processExecutable = func(pid int) (string, error) {
				if pid != parent.PID {
					t.Fatalf("executable lookup for %d, want invoking ancestor %d", pid, parent.PID)
				}
				return "/opt/codex/bin/codex", nil
			}
			processArgs = func(int) ([]string, error) { return row.args, nil }
			got := currentHarness(newCodexView(), "", "")
			if got.Kind != "codex" {
				t.Fatalf("harness = %+v, want Codex", got)
			}
			if row.daemon {
				if got.AppServer.Thread != "this-thread" || got.AppServer.Daemon.PID != 42 || got.PID != 0 || got.Cwd != "/project" {
					t.Fatalf("harness = %+v, want the loaded daemon thread", got)
				}
			} else if row.holder {
				if got.AppServer.Thread != "" || got.PID != parent.PID || got.holder() != parent {
					t.Fatalf("harness = %+v, holder = %+v; want invoking Codex ancestor %+v", got, got.holder(), parent)
				}
			} else if got.PID != 0 || got.AppServer.Thread != "" || got.holder().PID != 0 {
				t.Fatalf("harness = %+v, want unknown session liveness", got)
			}
		})
	}
}

// Review-s2 P2-8: a plain native attach in a link's own root kept the name of
// the share's binding and rewrote it with a cwd identity, so the share named
// a binding that another session could take. attach keeps the target
// identity when a link share already names a binding of this session.
func TestLinkSharedFindsTheSharesBindingOfThisSession(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	stateDir := filepath.Join(dir, stateDirName)
	if err := manifest.Write(manifest.DefaultPath(stateDir), manifest.File{
		SchemaVersion: manifest.RelaySchemaVersion, Layer: manifest.Layer,
		Links: []manifest.Link{{Name: "srv", URL: "wss://example.invalid/link", Shares: []manifest.LinkShare{{Binding: "claude-7", Consent: "passkey", Tools: "read"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	shared := binding.Binding{Root: dir, Target: "claude:7", NativeSession: "session-7", Name: "claude-7"}
	if err := binding.WriteNamed(shared); err != nil {
		t.Fatal(err)
	}
	if !linkShared(stateDir, binding.Binding{Root: dir, Target: "claude:7", NativeSession: "session-7"}) {
		t.Fatal("a plain attach of the session a link shares did not see the share")
	}
	if linkShared(stateDir, binding.Binding{Root: dir, Target: "claude:8", NativeSession: "session-8"}) {
		t.Fatal("another session counted as shared")
	}
	// Review-stack P2-B4: an unreadable manifest may hold the share, so it
	// keeps the target identity.
	unreadable := filepath.Join(t.TempDir(), stateDirName)
	if err := os.MkdirAll(manifest.DefaultPath(unreadable), 0o700); err != nil {
		t.Fatal(err)
	}
	if !linkShared(unreadable, binding.Binding{Root: dir, Target: "claude:8", NativeSession: "session-8"}) {
		t.Fatal("an unreadable manifest did not keep the target identity")
	}
}

// agent-message-queue-611.67: attach --self --link binds through the link's
// root, not AM_ROOT, and detach --self matched only AM_ROOT: it printed "Not
// connected." and kept the binding, with AM_ME set or not.
func TestDetachSelfRemovesABindingMadeThroughALinkRoot(t *testing.T) {
	for _, me := range []string{"", "agent"} {
		t.Run("AM_ME="+me, func(t *testing.T) {
			dir := canonicalTempDir(t)
			t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
			sessionRoot, linkRoot := filepath.Join(dir, "session"), filepath.Join(dir, "machine")
			for _, d := range []string{sessionRoot, linkRoot} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("AM_ROOT", sessionRoot)
			t.Setenv("AM_ME", me)
			if err := binding.Write(binding.Binding{Root: linkRoot, Target: "claude:7", NativeSession: "session-7", Name: "claude-7"}); err != nil {
				t.Fatal(err)
			}
			saved := selfIdentity
			t.Cleanup(func() { selfIdentity = saved })
			selfIdentity = func(string, string) (string, string, error) { return "claude:7", "session-7", nil }
			var out bytes.Buffer
			if code, err := detach([]string{"--self"}, &out, &out); err != nil || code != 0 {
				t.Fatalf("detach --self = %d, %v", code, err)
			}
			if all, _ := binding.List(); len(all) != 0 {
				t.Errorf("detach --self printed %q; binding %s (root %s) is still present", strings.TrimSpace(out.String()), all[0].Name, all[0].Root)
			}
		})
	}
}

// agent-message-queue-srt review: an attach of a CLAUDE_CONFIG_DIR session
// that failed before the fix left {"approve":true,"pid":N} in the manifest,
// and every later attach of that session was refused with "manifest already
// declares claude:N for another session; refusing".
func TestLiveRegisterReplacesAnEntryWhoseConfigDirLostThePid(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	pid := os.Getpid()
	reg, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "s-custom", "kind": "interactive"})
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", strconv.Itoa(pid)+".json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	root := canonicalTempDir(t)
	stateDir := filepath.Join(root, stateDirName)
	target := "claude:" + strconv.Itoa(pid)
	if _, err := persistAdapter(stateDir, manifest.Adapter{Kind: "claude", Target: target, Config: json.RawMessage(`{"approve":true,"pid":` + strconv.Itoa(pid) + `}`)}); err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	t.Cleanup(func() { _ = ep.Close(); _ = store.Close() })
	config, _ := json.Marshal(map[string]any{"approve": true, "config_dir": dir, "pid": pid})
	register := liveRegistrar(root, stateDir, "remote", ep)
	if _, err := register(ipc.RegisterRequest{Kind: "claude", Target: target, Config: config}); err != nil {
		t.Fatalf("register = %v", err)
	}
	f, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil || len(f.Adapters) != 1 || !sameJSON(f.Adapters[0].Config, config) {
		t.Fatalf("manifest = %+v, %v; want the entry with config_dir", f.Adapters, err)
	}
}

// Bead agent-message-queue-t97.4 (Pro finding 7): an attached session's
// handle stayed unregistered. A mailbox attach records the harness it found
// in the handle's existing mailbox, and never creates a missing one.
func TestMailboxAttachRecordsTheHarness(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	withMailbox := filepath.Join(dir, "p", ".agent-mail", "session1")
	without := filepath.Join(dir, "p", ".agent-mail", "session2")
	if err := fsq.EnsureAgentDirs(withMailbox, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(without, 0o700); err != nil {
		t.Fatal(err)
	}
	attachMailboxAs(t, withMailbox)
	rec, err := agentidentity.Read(withMailbox, "claude")
	if err != nil || rec.Harness != "other" {
		t.Fatalf("record = %+v err=%v, want harness other (no harness detected)", rec, err)
	}
	attachMailboxAs(t, without)
	if _, err := os.Lstat(filepath.Join(without, "agents", "claude")); !os.IsNotExist(err) {
		t.Fatalf("attach created a mailbox: %v", err)
	}
}

// PR #1201 review (Codex P2): the attach writer opened the current root path
// and wrote identity.json without authenticating that directory against the
// session pin, so a root replaced after the pin check received the record.
func TestAttachIdentitySkipsARootReplacedAfterThePinCheck(t *testing.T) {
	base := filepath.Join(canonicalTempDir(t), ".agent-mail")
	root := filepath.Join(base, "session1")
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatal(err)
	}
	rootID, err := fsq.StableTreeIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	baseID, err := fsq.StableTreeIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AM_BASE_ROOT", base)
	t.Setenv("AM_SESSION", "session1")
	t.Setenv("AM_ROOT_ID", rootID)
	t.Setenv("AM_BASE_ROOT_ID", baseID)
	if err := os.Rename(root, root+".selected"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatal(err)
	}
	recordAttachIdentity(io.Discard, root, "claude", "pi")
	if _, err := os.Lstat(filepath.Join(root, "agents", "claude", "identity.json")); !os.IsNotExist(err) {
		t.Fatalf("identity.json in the replacement root: %v", err)
	}
}

// Review of the identity-harness branch (P2): the native attach writer had no
// row. attach --self --native records the attached session's harness in the
// handle's existing mailbox of the session's root.
func TestNativeAttachRecordsTheHarness(t *testing.T) {
	tmp, err := os.MkdirTemp("", "a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_ME", "claude")
	for _, k := range []string{"AM_BASE_ROOT", "AM_SESSION", "AM_ROOT_ID", "AM_BASE_ROOT_ID"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, stateDirName)
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	ep.Register(fake.New("pi:s1", "e_1"))
	srv, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetRegistrar(liveRegistrar(root, stateDir, "remote", ep))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = ep.Close(); _ = store.Close() })

	savedFind, savedCwd, savedHolder := findSelf, processCwd, processHolder
	t.Cleanup(func() { findSelf, processCwd, processHolder = savedFind, savedCwd, savedHolder })
	findSelf = func(string, string) (registry.Candidate, error) {
		return registry.Candidate{Kind: "pi", Target: "pi:s1", NativeSession: "pi:s1", PID: 7}, nil
	}
	processCwd = func(int) (string, error) { return "/work/pi-proj", nil }
	processHolder = func(pid int) (launch.HolderIdentity, error) {
		return launch.HolderIdentity{PID: pid, ProcessStart: "1"}, nil
	}
	var stderr bytes.Buffer
	if code, err := attach([]string{"--self", "--native", "--json"}, io.Discard, &stderr); code != 0 || err != nil {
		t.Fatalf("attach --native: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
	rec, err := agentidentity.Read(root, "claude")
	if err != nil || rec.Harness != "pi" {
		t.Fatalf("record = %+v err=%v stderr=%q, want harness pi", rec, err, stderr.String())
	}
}

// Live check of a Codex --remote seat (2026-11-08): a session that is no AMQ
// participant ran attach --self --native and was refused with "--root or
// AM_ROOT is required". With no root, a native attach uses the machine root.
func TestNativeAttachWithoutARootUsesTheMachineRoot(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	for _, k := range []string{"AM_ROOT", "AM_ME", "AM_BASE_ROOT", "AM_SESSION", "AM_ROOT_ID", "AM_BASE_ROOT_ID"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	root, err := binding.EnsureMachineRoot()
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, stateDirName)
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	ep.Register(fake.New("pi:s1", "e_1"))
	srv, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetRegistrar(liveRegistrar(root, stateDir, "remote", ep))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = ep.Close(); _ = store.Close() })

	savedFind, savedCwd, savedHolder := findSelf, processCwd, processHolder
	t.Cleanup(func() { findSelf, processCwd, processHolder = savedFind, savedCwd, savedHolder })
	findSelf = func(string, string) (registry.Candidate, error) {
		return registry.Candidate{Kind: "pi", Target: "pi:s1", NativeSession: "pi:s1", PID: 7}, nil
	}
	processCwd = func(int) (string, error) { return "/work/pi-proj", nil }
	processHolder = func(pid int) (launch.HolderIdentity, error) {
		return launch.HolderIdentity{PID: pid, ProcessStart: "1"}, nil
	}
	var stderr bytes.Buffer
	if code, err := attach([]string{"--self", "--native", "--json"}, io.Discard, &stderr); code != 0 || err != nil {
		t.Fatalf("attach --native with no root: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
	all, err := binding.List()
	if err != nil || len(all) != 1 || !sameRoot(all[0].Root, root) {
		t.Fatalf("bindings = %+v, %v; want one bound through the machine root %s", all, err, root)
	}
}

// Review of the implicit native root: a symlinked machine root was accepted,
// because the default used the path without EnsureMachineRoot's checks.
func TestNativeAttachRefusesASymlinkedMachineRoot(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	for _, k := range []string{"AM_ROOT", "AM_ME", "AM_BASE_ROOT", "AM_SESSION", "AM_ROOT_ID", "AM_BASE_ROOT_ID"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	elsewhere := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".amq", "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".amq", "remote", "root")); err != nil {
		t.Fatal(err)
	}
	savedFind := findSelf
	t.Cleanup(func() { findSelf = savedFind })
	findSelf = func(string, string) (registry.Candidate, error) {
		return registry.Candidate{Kind: "pi", Target: "pi:s1", NativeSession: "pi:s1", PID: 7}, nil
	}
	_, err = attach([]string{"--self", "--native", "--json"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is not a plain directory") {
		t.Fatalf("attach through a symlinked machine root: err=%v; want the symlink refusal", err)
	}
}

// shortHome is a HOME short enough that the machine root's endpoint socket
// fits the Unix sun_path limit. The default temp directory on a macOS CI
// runner is too deep (macOS CI on PR #1415: a 117-byte socket path).
func shortHome(t *testing.T) string {
	t.Helper()
	base := ""
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	home, err := os.MkdirTemp(base, "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if home, err = filepath.EvalSymlinks(home); err != nil {
		t.Fatal(err)
	}
	return home
}
