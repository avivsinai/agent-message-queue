package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// attachReadyTimeout bounds the wait for an endpoint that attach started.
const attachReadyTimeout = 20 * time.Second

// attach binds the per-user Buzz agent to the session the owner is typing
// in (bead agent-message-queue-611.31). Typing the command is the sharing
// choice, so the invoking session is found exactly, never guessed from a
// discovery list: Claude by its process ancestry, Codex by CODEX_THREAD_ID.
func attach(args []string, stdout, stderr io.Writer, probe ...*jsonProbe) (int, error) {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	self := fs.Bool("self", false, "bind the session this command runs in")
	nativeMode := fs.Bool("native", false, "drive the exact native session through amq-remote, not the AMQ mailbox")
	me := fs.String("me", os.Getenv("AM_ME"), "AMQ handle of this session (default AM_ME)")
	name := fs.String("name", "", "binding name, which names this session's Buzz agent (default <handle>-<project>, or the native target)")
	linkName := fs.String("link", "", "share this session with the linked server of that name (implies --native)")
	consent := fs.String("consent", "passkey", "with --link: passkey, or local to also confirm each task on this machine")
	tools := fs.String("tools", "read", "with --link: the server tool profile agents in this session may call; empty for none")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if !*self {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "attach needs --self")
	}
	linkFlags := false
	fs.Visit(func(f *flag.Flag) { linkFlags = linkFlags || f.Name == "consent" || f.Name == "tools" })
	if *linkName == "" && linkFlags {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--consent and --tools apply only with --link")
	}
	if *linkName != "" {
		if err := linkio.ValidName(*linkName); err != nil {
			return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
		}
		// Check the share before anything is attached or written.
		if err := manifest.ValidLinkShare(manifest.LinkShare{Binding: "session", Consent: *consent, Tools: *tools}); err != nil {
			return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
		}
		*nativeMode = true // a linked server reaches a native session only
		stateDir, err := c.stateDir()
		if err != nil {
			return protocol.ExitUsage, err
		}
		if err := requireLinkEndpoint(stateDir); err != nil {
			return protocol.ExitActionRequired, err
		}
	}
	if !*nativeMode {
		return attachMailbox(c.root, strings.TrimSpace(*me), strings.TrimSpace(*name), c.json, stdout)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return protocol.ExitUsage, err
	}
	cand, err := selfCandidate(c.root, stateDir)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	// The session's own identity is resolved before anything is registered,
	// written or started, so a refusal leaves no side effect.
	want, err := selfNativeSession(cand)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	reg := ipc.RegisterRequest{Kind: cand.Kind, Target: cand.Target, Config: cand.Config}
	if cand.Kind == "claude" && *linkName == "" {
		// A Claude session shows its tool approvals in the Buzz DM, where
		// the owner can deny them (bead agent-message-queue-611.42.2). A
		// session shared with a link never gains answering authority: no
		// remote source answers the agent's prompts.
		if reg.Config, err = withApprove(cand.Config); err != nil {
			return protocol.ExitActionRequired, err
		}
	}
	session, err := registerTarget(stateDir, reg)
	if isEndpointUnreachable(err) {
		// No endpoint yet: record the target where startup reads it, then
		// start a supervised endpoint and register again once it answers.
		if err = persistAdapter(stateDir, manifest.Adapter{Kind: reg.Kind, Target: reg.Target, Config: reg.Config}); err == nil {
			if err = startEndpoint(c.root, stateDir); err == nil {
				session, err = awaitRegister(stateDir, reg)
			}
		}
	}
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	native, err := nativeSessionOf(stateDir, cand.Target)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	// The endpoint's answer is checked against the identity resolved above,
	// independently, so a target name reused for another session is never
	// pinned as this one (codex #885 P1 #2).
	if native != want {
		return protocol.ExitActionRequired, fmt.Errorf("the endpoint's %s is attached to another session; refusing to bind", cand.Target)
	}
	display := cand.Display
	if display == "" {
		display = session.DisplayName
	}
	bindName, explicit := nativeBindingName(*name, cand.Target)
	nb := binding.Binding{Root: c.root, Target: cand.Target, NativeSession: native, Display: display, Name: bindName}
	if err := writeBinding(nb, explicit, *linkName != ""); err != nil {
		return protocol.ExitActionRequired, err
	}
	if *linkName != "" {
		if err := shareWithLink(stateDir, *linkName, manifest.LinkShare{Binding: nb.Name, Consent: *consent, Tools: *tools}); err != nil {
			return protocol.ExitActionRequired, err
		}
		if c.json {
			emitJSON(stdout, map[string]any{"connected": true, "name": nb.Name, "root": nb.Root, "target": nb.Target, "link": *linkName})
			return 0, nil
		}
		say(stdout, "Shared %s (%s) with link %s as %s (consent: %s, tools: %s).", nonEmpty(display, cand.Target), cand.Target, *linkName, nb.Name, *consent, nonEmpty(*tools, "none"))
		return 0, nil
	}
	if c.json {
		emitJSON(stdout, map[string]any{"connected": true, "name": nb.Name, "root": nb.Root, "target": nb.Target})
		return 0, nil
	}
	say(stdout, "Connected: %s (%s) as session %s. DM its Buzz agent \"AMQ: %s\".", nonEmpty(display, cand.Target), cand.Target, nb.Name, nb.Name)
	return 0, nil
}

// attachMailbox binds the Buzz agent to this session's AMQ handle (bead
// agent-message-queue-611.36). Each DM becomes an AMQ message to the handle;
// no endpoint, hook, or wake is required, because noticing the message is
// the handle owner's business.
func attachMailbox(root, handle, name string, asJSON bool, stdout io.Writer) (int, error) {
	if root == "" || handle == "" {
		return protocol.ExitActionRequired, errors.New("this session is not an AMQ participant (AM_ROOT and AM_ME are unset); join AMQ, or use attach --self --native")
	}
	if !filepath.IsAbs(root) {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--root must be absolute")
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return protocol.ExitActionRequired, fmt.Errorf("AMQ root %s is not a directory", root)
	}
	// The inherited session pin must name this root, exactly as a direct
	// amq-acp mailbox checks it (codex #895 P1 #4).
	if err := acp.VerifySessionPin(filepath.Clean(root)); err != nil {
		return protocol.ExitActionRequired, err
	}
	explicit := name != ""
	if !explicit {
		name = binding.SanitizeName(handle + "-" + projectOf(root))
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: handle, Display: handle, Name: name}
	if err := listBuzzInRoster(root); err != nil {
		return protocol.ExitActionRequired, err
	}
	if err := writeBinding(b, explicit, false); err != nil {
		return protocol.ExitActionRequired, err
	}
	if asJSON {
		emitJSON(stdout, map[string]any{"connected": true, "name": name, "root": root, "handle": handle})
		return 0, nil
	}
	say(stdout, "Connected: AMQ handle %s at %s as session %s. DM its Buzz agent \"AMQ: %s\".", handle, root, name, name)
	return 0, nil
}

func emitJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// listBuzzInRoster adds the Buzz agent's handle to the root's config.json
// agents list, so a reply to a Buzz DM routes without a "may not be read"
// warning or a --strict refusal (bead agent-message-queue-za4). A root with
// no config.json is left as it is, and a handle already listed is a no-op.
// Only the mailbox attach calls it. The root is opened once as a capability,
// authenticated against the inherited session pin, and updated through that
// same capability, so a directory swapped in after the check is never written.
func listBuzzInRoster(root string) error {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = dr.Close() }()
	if err := acp.VerifySessionPinOn(dr); err != nil {
		return err
	}
	if _, err := dr.ReadFile("meta/config.json"); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err = config.EnsureAgentOn(dr, "buzz")
	return err
}

// nativeBindingName is the binding name for a native attach and whether the
// caller chose it. The name and the choice come from one trimmed value, so a
// whitespace-only --name is the default, never an explicit replacement.
func nativeBindingName(flagValue, target string) (string, bool) {
	trimmed := strings.TrimSpace(flagValue)
	return nonEmpty(trimmed, binding.SanitizeName(target)), trimmed != ""
}

// writeBinding adds the named binding and removes any other binding for the
// same session, so one session is one Buzz agent (bead
// agent-message-queue-611.39). A defaulted name never replaces another
// session's binding (bead agent-message-queue-94w); an explicit --name
// is the caller's choice and replaces.
//
// With followLink (attach --self --link), the name also moves to b when it
// pins the same root and target under another native session: the share
// keeps its name after a session change (E2E kit F3, ruling ww). attach
// reaches this write only after the endpoint confirmed the target is attached
// to b's session, and one target reports one native session, so the old
// session is no longer the attached one. An old session resumed in another
// process is another target, so another default name.
func writeBinding(b binding.Binding, explicitName, followLink bool) error {
	var err error
	switch {
	case explicitName:
		err = binding.WriteNamed(b)
	case followLink:
		err = binding.WriteNamedIf(b, func(old binding.Binding) bool { return old.Same(b) || movedTarget(old, b) })
	default:
		err = binding.WriteNamedNew(b)
	}
	if err != nil {
		return err
	}
	_, err = binding.RemoveMatching(func(o binding.Binding) bool { return o.Same(b) && o.Name != b.Name })
	return err
}

// movedTarget reports whether old pins the same root and native target as b
// under another native session. A mailbox binding pins no native session.
func movedTarget(old, b binding.Binding) bool {
	return !old.Mailbox() && !b.Mailbox() && filepath.Clean(old.Root) == filepath.Clean(b.Root) &&
		old.Target == b.Target && old.NativeSession != b.NativeSession
}

// projectOf is the project directory name of an AMQ root such as
// <project>/.agent-mail/<session>.
func projectOf(root string) string {
	for dir := filepath.Clean(root); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) == ".agent-mail" {
			return filepath.Base(filepath.Dir(dir))
		}
	}
	return filepath.Base(root)
}

// detach ends bindings. It needs a scope: --self unbinds only the binding
// that names this session, so one session's off never unbinds another;
// --name unbinds the binding of that name; --all unbinds every binding.
func detach(args []string, stdout, stderr io.Writer, probe ...*jsonProbe) (int, error) {
	fs := flag.NewFlagSet("detach", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	self := fs.Bool("self", false, "unbind only if this session is the bound one")
	name := fs.String("name", "", "unbind only the binding with this name")
	all := fs.Bool("all", false, "unbind every binding")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	*name = strings.TrimSpace(*name)
	scopes := 0
	for _, set := range []bool{*self, *name != "", *all} {
		if set {
			scopes++
		}
	}
	if scopes != 1 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "detach needs exactly one of --self, --name <name> or --all")
	}
	var match func(binding.Binding) bool
	// A native binding made outside AMQ used a fallback root; detach finds it
	// in the binding itself, so off needs no --root (codex #895 P2 #5).
	if *self && c.root == "" {
		if all, err := binding.List(); err == nil {
			for _, b := range all {
				if !b.Mailbox() {
					c.root = b.Root
					break
				}
			}
		}
	}
	if *self && os.Getenv("AM_ME") != "" && c.root != "" {
		mine := binding.Binding{Carrier: binding.CarrierMailbox, Root: c.root, Handle: strings.TrimSpace(os.Getenv("AM_ME"))}
		native := func(binding.Binding) bool { return false }
		if stateDir, err := c.stateDir(); err == nil {
			if target, id, err := selfIdentity(c.root, stateDir); err == nil {
				nb := binding.Binding{Root: c.root, Target: target, NativeSession: id}
				native = nb.Same
			}
		}
		match = func(b binding.Binding) bool { return mine.Same(b) || native(b) }
	} else if *self {
		stateDir, err := c.stateDir()
		if err != nil {
			return protocol.ExitUsage, err
		}
		target, native, err := selfIdentity(c.root, stateDir)
		if err != nil {
			return protocol.ExitActionRequired, err
		}
		mine := binding.Binding{Root: c.root, Target: target, NativeSession: native}
		match = mine.Same
	}
	if *name != "" {
		match = func(b binding.Binding) bool { return b.Name == *name }
	} else if match == nil {
		match = func(binding.Binding) bool { return true }
	}
	removed, err := binding.RemoveMatching(match)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if c.json {
		if removed == nil {
			removed = []string{}
		}
		emitJSON(stdout, map[string]any{"removed": removed})
		return 0, nil
	}
	if len(removed) == 0 {
		say(stdout, "Not connected.")
		return 0, nil
	}
	say(stdout, "Disconnected. The session keeps running; Buzz DMs now answer \"Not connected\".")
	return 0, nil
}

// selfCandidate finds the invoking native session among discovered ones.
func selfCandidate(root, stateDir string) (registry.Candidate, error) {
	cands, diags := registry.Discover(context.Background(), registry.DiscoverRequest{Root: root, StateDir: stateDir})
	if thread := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")); thread != "" {
		for _, cand := range cands {
			if cand.Kind != "codex" {
				continue
			}
			var cfg struct {
				Thread string `json:"thread"`
			}
			if json.Unmarshal(cand.Config, &cfg) == nil && cfg.Thread == thread {
				return cand, nil
			}
		}
		// A daemon AMQ cannot reach says nothing about the thread: name the
		// failure, not "not loaded". Codex's sandbox denies the daemon socket
		// to a tool call (agent-message-queue-611.65).
		for _, d := range diags {
			switch {
			case d.Kind != "codex":
			case errors.Is(d.Err, fs.ErrPermission):
				return registry.Candidate{}, fmt.Errorf("cannot reach the Codex app-server daemon: %v; a Codex sandbox blocks its socket, so run this command outside the sandbox", d.Err)
			default:
				return registry.Candidate{}, fmt.Errorf("cannot reach the Codex app-server daemon: %v; start it with `codex app-server daemon start`, then Codex with `codex --remote unix://`", d.Err)
			}
		}
		return registry.Candidate{}, fmt.Errorf("codex thread %s is not loaded in the app-server daemon; start Codex with `codex --remote unix://`", thread)
	}
	ancestors, err := ancestorPIDs(os.Getpid())
	if err != nil {
		return registry.Candidate{}, err
	}
	for _, pid := range ancestors {
		var matched []registry.Candidate
		for _, cand := range cands {
			if candidatePID(cand) == pid {
				matched = append(matched, cand)
			}
		}
		switch len(matched) {
		case 0:
			continue
		case 1:
			return matched[0], nil
		}
		// Two sessions report the same process: never guess which one this is.
		targets := make([]string, len(matched))
		for i, m := range matched {
			targets[i] = m.Target
		}
		return registry.Candidate{}, fmt.Errorf("ambiguous: %s all report pid %d; refusing to guess", strings.Join(targets, ", "), pid)
	}
	return registry.Candidate{}, errors.New("cannot identify the session this runs in; run it from inside a Claude Code, Codex or pi (Amit) session")
}

// candidatePID is the process that runs a candidate's session: Claude's
// from its config, a pi (Amit) chat's from its bridge's liveness (its tools
// run as children of that process); 0 for kinds found another way.
func candidatePID(cand registry.Candidate) int {
	switch cand.Kind {
	case "claude":
		var cfg struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(cand.Config, &cfg) == nil {
			return cfg.PID
		}
	case "pi":
		return cand.PID
	}
	return 0
}

// selfIdentity is the invoking session's target and native session. It is a
// variable so detach tests can supply a fixed session instead of the
// machine's ambient one.
var selfIdentity = func(root, stateDir string) (string, string, error) {
	cand, err := selfCandidate(root, stateDir)
	if err != nil {
		return "", "", err
	}
	native, err := selfNativeSession(cand)
	if err != nil {
		return "", "", err
	}
	return cand.Target, native, nil
}

// selfNativeSession resolves the invoking session's native identity without
// the endpoint: the Claude registry entry for its pid, or the Codex thread.
func selfNativeSession(cand registry.Candidate) (string, error) {
	switch cand.Kind {
	case "claude":
		var cfg struct {
			PID int `json:"pid"`
		}
		if err := json.Unmarshal(cand.Config, &cfg); err != nil || cfg.PID <= 0 {
			return "", fmt.Errorf("claude candidate %s has no pid", cand.Target)
		}
		return claude.SessionIDForPID(cfg.PID)
	case "codex":
		var cfg struct {
			Thread string `json:"thread"`
		}
		if err := json.Unmarshal(cand.Config, &cfg); err != nil || cfg.Thread == "" {
			return "", fmt.Errorf("codex candidate %s has no thread", cand.Target)
		}
		return cfg.Thread, nil
	case "pi":
		// Read from the chat's own bridge during discovery.
		if cand.NativeSession == "" {
			return "", fmt.Errorf("the pi bridge of %s publishes no session id; update the pi bridge extension", cand.Target)
		}
		return cand.NativeSession, nil
	}
	return "", fmt.Errorf("cannot verify a %s session", cand.Kind)
}

// ancestorPIDs lists the parent chain of pid, nearest first.
func ancestorPIDs(pid int) ([]int, error) {
	var out []int
	for i := 0; i < 64 && pid > 1; i++ {
		raw, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return out, nil
		}
		parent, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || parent <= 1 {
			return out, nil
		}
		out = append(out, parent)
		pid = parent
	}
	return out, nil
}

func registerTarget(stateDir string, reg ipc.RegisterRequest) (protocol.Session, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Register: &reg})
	if err != nil {
		return protocol.Session{}, err
	}
	if err := resp.AsError(); err != nil {
		return protocol.Session{}, err
	}
	var s protocol.Session
	if err := json.Unmarshal(resp.Reply, &s); err != nil {
		return protocol.Session{}, err
	}
	return s, nil
}

func awaitRegister(stateDir string, reg ipc.RegisterRequest) (protocol.Session, error) {
	deadline := time.Now().Add(attachReadyTimeout)
	for {
		s, err := registerTarget(stateDir, reg)
		if !isEndpointUnreachable(err) || time.Now().After(deadline) {
			return s, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func nativeSessionOf(stateDir, target string) (string, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Native: &ipc.NativeQuery{TargetID: target}})
	if err != nil {
		return "", err
	}
	if err := resp.AsError(); err != nil {
		return "", err
	}
	var reply ipc.NativeReply
	if err := json.Unmarshal(resp.Reply, &reply); err != nil || reply.NativeSession == "" {
		return "", fmt.Errorf("target %s reports no native session", target)
	}
	return reply.NativeSession, nil
}

// persistAdapter adds a manifest entry for a so a restart attaches it again.
// An existing entry for the target is kept as it is.
func persistAdapter(stateDir string, a manifest.Adapter) error {
	if !lock.AdvisoryLockAvailable() {
		return errors.New("refusing to update the manifest without an advisory file lock")
	}
	path := manifest.DefaultPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(path+".lock", func() error {
		f, err := manifest.Load(path)
		if err != nil {
			return err
		}
		for _, existing := range f.Adapters {
			if existing.Target != a.Target {
				continue
			}
			// The same target name with another kind or config names another
			// session; it is refused, never silently kept (codex #885 P1 #2).
			if existing.Kind != a.Kind || existing.Epoch != a.Epoch {
				return protocol.Refuse(protocol.CodeInvalid, "manifest already declares %s for another session; refusing", a.Target)
			}
			if sameJSON(existing.Config, a.Config) {
				return nil
			}
			// The same Claude session attached before approvals existed
			// gains approve; anything else names another session.
			if upgraded, err := withApprove(existing.Config); a.Kind != "claude" || err != nil || !sameJSON(upgraded, a.Config) {
				return protocol.Refuse(protocol.CodeInvalid, "manifest already declares %s for another session; refusing", a.Target)
			}
			for i := range f.Adapters {
				if f.Adapters[i].Target == a.Target {
					f.Adapters[i].Config = a.Config
				}
			}
			if err := manifest.Validate(f); err != nil {
				return err
			}
			return manifest.Write(path, f)
		}
		f.Adapters = append(f.Adapters, a)
		if err := manifest.Validate(f); err != nil {
			return err
		}
		return manifest.Write(path, f)
	})
}

// liveRegistrar attaches one adapter to the running endpoint the way startup
// attaches manifest entries, after persisting it for restarts.
// A Claude target gets its native approval pin from pin when non-nil.
func liveRegistrar(root, stateDir string, ep *core.Endpoint, pin func(manifest.Adapter)) ipc.Registrar {
	var mu sync.Mutex
	return func(r ipc.RegisterRequest) (protocol.Session, error) {
		mu.Lock()
		defer mu.Unlock()
		a := manifest.Adapter{Kind: r.Kind, Target: r.Target, Config: r.Config, Epoch: r.Epoch}
		if err := persistAdapter(stateDir, a); err != nil {
			return protocol.Session{}, err
		}
		if _, ok := ep.AttachmentOf(r.Target); !ok {
			out := registry.Build(context.Background(), root, stateDir, manifest.File{Adapters: []manifest.Adapter{a}})
			if len(out) != 1 || out[0].Refusal != nil {
				reason := "no adapter built"
				if len(out) == 1 {
					reason = out[0].Refusal.Error()
				}
				return protocol.Session{}, protocol.Refuse(protocol.CodeUnsupported, "attach %s: %s", r.Target, reason)
			}
			ep.Register(out[0].Attachment)
			if pin != nil {
				pin(a)
			}
		}
		for _, s := range ep.Sessions() {
			if s.TargetID == r.Target {
				return s, nil
			}
		}
		return protocol.Session{}, protocol.Refuse(protocol.CodeNotFound, "target %s did not attach", r.Target)
	}
}

// withApprove is a Claude adapter config with approve set.
func withApprove(config json.RawMessage) (json.RawMessage, error) {
	cfg := map[string]any{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("claude adapter config: %w", err)
		}
	}
	cfg["approve"] = true
	return json.Marshal(cfg)
}

// sameJSON compares two config blocks by value.
func sameJSON(a, b json.RawMessage) bool {
	var va, vb any
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	ja, _ := json.Marshal(va)
	jb, _ := json.Marshal(vb)
	return string(ja) == string(jb)
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
