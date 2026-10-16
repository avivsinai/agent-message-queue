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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
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
	name := fs.String("name", "", "binding name, which names this session's Buzz agent (default <handle>-<project>-<session>, or the native target)")
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
	}
	if !*nativeMode {
		return attachMailbox(c.root, strings.TrimSpace(*me), strings.TrimSpace(*name), c.json, stdout)
	}
	// A linked share runs in the endpoint of the link's root, known once the
	// session is; discovery reads only the session's root (pi), no state dir.
	var stateDir string
	var err error
	if *linkName == "" {
		if stateDir, err = c.stateDir(); err != nil {
			return protocol.ExitUsage, err
		}
	}
	cand, err := findSelf(c.root, stateDir)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if cand.Kind == "claude" {
		if err := requireStopHook(); err != nil {
			return protocol.ExitActionRequired, err
		}
	}
	// The session's own identity is resolved before anything is registered,
	// written or started, so a refusal leaves no side effect.
	want, err := selfNativeSession(cand)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if *linkName == "" {
		// A link share binds this session through the link's root; a plain
		// native attach must not add a second binding through another root.
		if b, err := boundElsewhere(c.root, cand.Target, want); err != nil {
			return protocol.ExitActionRequired, err
		} else if b != nil {
			return protocol.ExitActionRequired, protocol.Refuse(protocol.CodeUnsupported, "this session is attached through %s (binding %s); run amq-remote detach --name %s, then attach --self --native again", b.Root, b.Name, b.Name)
		}
	} else {
		root, err := linkAttachRoot(c, *linkName, cand, want)
		if err != nil {
			return protocol.ExitCode(err), err
		}
		c.root, stateDir = root, filepath.Join(root, stateDirName)
		if err := requireLinkEndpoint(stateDir); err != nil {
			return protocol.ExitActionRequired, err
		}
		pin, err := linkio.LoadPin(stateDir, *linkName)
		if err != nil {
			return protocol.ExitActionRequired, fmt.Errorf("link %s is not linked on this machine (%v); run amq-remote link add again", *linkName, err)
		}
		if err := checkToolProfile(pin, *linkName, *tools); err != nil {
			return protocol.ExitUsage, err
		}
		if *consent == "local" && !linkio.HasLocalKey(stateDir, *linkName) {
			return protocol.ExitActionRequired, protocol.Refuse(protocol.CodeUnsupported, "consent local needs the link's local passkey: run amq-remote link local-key %s first", *linkName)
		}
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
	h := nativeHarness(cand)
	place := nonEmpty(display, cand.Target)
	if h.Cwd != "" {
		place = filepath.Base(h.Cwd)
	}
	machine := binding.Machine()
	nb := binding.Binding{Root: c.root, Target: cand.Target, NativeSession: native, Display: display, Name: bindName,
		Harness: h.Kind, Holder: h.holder(), Label: sessionLabel(h.Kind, place, machine), Machine: machine, AppServer: h.AppServer}
	// The session identity is the harness, root and working directory, so the
	// same session coming back gets its name and agent back (ruling 2). A link
	// share keeps its target-derived identity: the share names its binding,
	// and another process must never take that name over. That holds for a
	// plain attach of a session a link already shares too: it keeps the
	// share's binding as the share made it.
	if *linkName == "" && h.Cwd != "" && !linkShared(stateDir, nb) {
		nb.Identity = binding.NativeIdentity(h.Kind, c.root, h.Cwd)
		if !explicit {
			nb.Name = binding.SanitizeName(h.Kind + "-" + filepath.Base(h.Cwd))
		}
	}
	if nb.Name, err = writeBinding(nb, explicit, *linkName != ""); err != nil {
		return protocol.ExitActionRequired, err
	}
	pruneAfterAttach()
	if *linkName != "" {
		if err := shareWithLink(stateDir, *linkName, manifest.LinkShare{Binding: nb.Name, Consent: *consent, Tools: *tools}); err != nil {
			return protocol.ExitActionRequired, err
		}
		// No approval pin is written for a link share (only a Buzz relay
		// share pins one), so the approval hook leaves every tool permission
		// prompt to the session's own dialog.
		if c.json {
			emitJSON(stdout, map[string]any{"connected": true, "name": nb.Name, "root": nb.Root, "target": nb.Target, "link": *linkName, "approvals": "terminal"})
			return 0, nil
		}
		say(stdout, "Shared %s (%s) with link %s as %s (consent: %s, tools: %s).", nonEmpty(display, cand.Target), cand.Target, *linkName, nb.Name, *consent, nonEmpty(*tools, "none"))
		say(stdout, "Tool approvals for this share are answered in the session's terminal; the linked server cannot answer them, so a task that needs one waits there.")
		return 0, nil
	}
	if c.json {
		emitJSON(stdout, map[string]any{"connected": true, "name": nb.Name, "root": nb.Root, "target": nb.Target})
		return 0, nil
	}
	say(stdout, "Connected: %s (%s) as session %s. DM its Buzz agent \"AMQ: %s\".", nonEmpty(display, cand.Target), cand.Target, nb.Name, nb.Name)
	return 0, nil
}

// linkAttachRoot is the root attach --self --link shares the session through:
// the root that holds the link, so the target registration, the binding and
// the share all name the link's own endpoint. It refuses a pi session whose
// AMQ root is another root, because only the endpoint of a pi session's own
// root can drive it, and a session already bound natively through another
// root, because two bindings would route into one session through two
// endpoints.
func linkAttachRoot(c *common, link string, cand registry.Candidate, want string) (string, error) {
	sessionRoot := c.root
	root, _, err := c.resolveLink(link)
	if err != nil {
		return "", err
	}
	if cand.Kind == "pi" && (sessionRoot == "" || !sameRoot(root, sessionRoot)) {
		return "", protocol.Refuse(protocol.CodeUnsupported, "this pi session is served by the endpoint of its AMQ root %s, and link %s is in %s. Link that root under another name: amq-remote link add OTHER URL --code CODE --root %s", sessionRoot, link, root, sessionRoot)
	}
	if b, err := boundElsewhere(root, cand.Target, want); err != nil {
		return "", err
	} else if b != nil {
		return "", protocol.Refuse(protocol.CodeUnsupported, "this session is attached through %s (binding %s); run amq-remote detach --self, then attach --self --link %s again", b.Root, b.Name, link)
	}
	if m, err := machineRoot(); err == nil && sameRoot(root, m) {
		if _, err := ensureMachineRoot(); err != nil {
			return "", err
		}
	}
	return root, nil
}

// boundElsewhere returns a native binding of this session (target and native
// session) that names a root other than root, or nil. binding.Same treats
// two roots as two sessions, so without this check one session would keep
// two routed bindings, one through each root's endpoint.
func boundElsewhere(root, target, native string) (*binding.Binding, error) {
	all, err := binding.List()
	if err != nil {
		return nil, err
	}
	for _, b := range all {
		if !b.Mailbox() && b.Target == target && b.NativeSession == native && !sameRoot(b.Root, root) {
			return &b, nil
		}
	}
	return nil, nil
}

// checkToolProfile refuses a --tools profile the link's server does not
// grant, when its link info lists the profiles it grants. No --tools needs
// no profile.
func checkToolProfile(pin linkio.Pin, link, tools string) error {
	if pin.ToolProfiles == nil || tools == "" || slices.Contains(*pin.ToolProfiles, tools) {
		return nil
	}
	if len(*pin.ToolProfiles) == 0 {
		return protocol.Refuse(protocol.CodeInvalid, "link %s's server grants no tool profiles; use --tools \"\"", link)
	}
	return protocol.Refuse(protocol.CodeInvalid, "link %s's server grants tool profiles %s; --tools %s is not one of them", link, strings.Join(*pin.ToolProfiles, ", "), tools)
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
		name = mailboxBindingName(handle, root)
	}
	h := detectHarness()
	project, session := amqPlace(root)
	machine := binding.Machine()
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: handle, Display: handle, Name: name,
		Harness: h.Kind, Holder: h.holder(), Label: sessionLabel(nonEmpty(h.Kind, handle), strings.TrimSuffix(project+"/"+session, "/"), machine), Machine: machine,
		AppServer: h.AppServer}
	if err := listBuzzInRoster(root); err != nil {
		return protocol.ExitActionRequired, err
	}
	name, err := writeBinding(b, explicit, false)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	pruneAfterAttach()
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

// writeBinding adds the named binding, removes any other binding for the
// same session, so one session is one Buzz agent (bead
// agent-message-queue-611.39), and returns the name written. A defaulted name
// never replaces another session's binding (bead agent-message-queue-94w); an
// explicit --name is the caller's choice and replaces. A defaulted name is the
// session's own: the name its identity already holds, else the first of
// b.Name, b.Name-2, ... that no other session holds (binding.WriteSession).
//
// With followLink (attach --self --link), the name also moves to b when it
// pins the same root and target under another native session: the share
// keeps its name after a session change (E2E kit F3, ruling ww). attach
// reaches this write only after the endpoint confirmed the target is attached
// to b's session, and one target reports one native session, so the old
// session is no longer the attached one. An old session resumed in another
// process is another target, so another default name.
func writeBinding(b binding.Binding, explicitName, followLink bool) (string, error) {
	var err error
	switch {
	case explicitName:
		err = binding.WriteNamed(b)
	case followLink:
		err = binding.WriteNamedIf(b, func(old binding.Binding) bool { return old.Same(b) || movedTarget(old, b) })
	default:
		b.Name, err = binding.WriteSession(b, b.Name, inspectOutsideLock())
	}
	if err != nil {
		return "", err
	}
	_, err = binding.Retire(func(o binding.Binding) bool { return o.Same(b) && o.Name != b.Name })
	return b.Name, err
}

// movedTarget reports whether old pins the same root and native target as b
// under another native session. A mailbox binding pins no native session.
func movedTarget(old, b binding.Binding) bool {
	return !old.Mailbox() && !b.Mailbox() && filepath.Clean(old.Root) == filepath.Clean(b.Root) &&
		old.Target == b.Target && old.NativeSession != b.NativeSession
}

// mailboxBindingName is the default binding name of a mailbox session:
// <handle>-<project>-<session> for an AMQ root <project>/.agent-mail/<session>,
// so two AMQ sessions of one project never compute the same name, else
// <handle>-<project>.
func mailboxBindingName(handle, root string) string {
	project, session := amqPlace(root)
	return binding.SanitizeName(strings.Join([]string{handle, project, session}, "-"))
}

// amqPlace is the project and AMQ session of a root
// <project>/.agent-mail/<session>. A root outside that layout has no session,
// and its base name is the project.
func amqPlace(root string) (project, session string) {
	for dir := filepath.Clean(root); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) == ".agent-mail" {
			return filepath.Base(filepath.Dir(dir)), session
		}
		session = filepath.Base(dir)
	}
	return filepath.Base(root), ""
}

// maxLabel bounds a label to the longest Buzz agent display name.
const maxLabel = 120

// sessionLabel is the human name of a session and of its Buzz agent,
// "<harness> · <place> @ <machine>", at most maxLabel characters. A long
// place is cut, never the machine, which tells two machines' agents apart.
func sessionLabel(harness, place, machine string) string {
	head, tail := harness+" · ", ""
	if machine != "" {
		tail = " @ " + machine
	}
	room := max(maxLabel-len([]rune(head))-len([]rune(tail)), 0)
	if r := []rune(place); len(r) > room {
		place = string(r[:room])
	}
	label := []rune(head + place + tail)
	return string(label[:min(len(label), maxLabel)])
}

// harnessSession is what attach knows of the harness a session runs in.
type harnessSession struct {
	Kind string // claude, codex or pi; empty when not detected
	PID  int    // the harness process; 0 when not known
	Cwd  string // the session's working directory; empty when not known
	// AppServer is a Codex session's thread, which proves its liveness in
	// place of a process.
	AppServer binding.AppServerThread
}

// holder is the recorded identity of the harness process, or zero when it
// cannot be read, which leaves the session's liveness unknown.
func (h harnessSession) holder() launch.HolderIdentity {
	if h.PID <= 0 {
		return launch.HolderIdentity{}
	}
	id, err := launch.HolderOf(h.PID)
	if err != nil {
		return launch.HolderIdentity{}
	}
	return id
}

// nativeHarness is the harness of a native session, with the session's own
// working directory, never that of the tool call attach runs in: a Claude
// session's from its registry entry, a pi session's from its process, and a
// Codex thread's from the app-server daemon. A Codex thread runs in that
// daemon, so it has no session process; its thread is recorded instead.
func nativeHarness(cand registry.Candidate) harnessSession {
	h := harnessSession{Kind: cand.Kind, PID: candidatePID(cand)}
	switch cand.Kind {
	case "claude":
		if s, err := claudeSession(h.PID); err == nil {
			h.Cwd = s.Cwd
		}
	case "pi":
		h.Cwd, _ = processCwd(h.PID)
	case "codex":
		var cfg struct {
			Socket string `json:"socket"`
			Thread string `json:"thread"`
		}
		if json.Unmarshal(cand.Config, &cfg) == nil && cfg.Socket != "" && cfg.Thread != "" {
			h.AppServer = loadedThread(cfg.Socket, cfg.Thread)
			h.Cwd, _ = codexThreadCwd(cfg.Socket, cfg.Thread)
		}
	}
	return h
}

// Harness inspection reads ~/.claude, the app-server daemon, and other
// processes. They are variables so tests read none of them.
var (
	claudeSession      = claude.SessionForPID
	processCwd         = launch.ProcessCwd
	codexThreadCwd     = codex.ThreadCwd
	codexLoadedThreads = func(sock string) ([]string, error) { return codex.LoadedThreadsWithin(sock, 2*time.Second) }
	codexDefaultSocket = codex.DefaultControlSocket
	codexDaemonPID     = codex.DaemonPID
	processHolder      = launch.HolderOf
)

// loadedThread is the thread to record for a Codex session: the thread with
// the daemon process that has it loaded now. Unless that daemon confirms the
// thread is loaded, it records nothing, and the session's liveness stays
// unknown: the session may run on another app-server, which this daemon
// does not answer for (review-1070 P1-B1).
func loadedThread(sock, thread string) binding.AppServerThread {
	daemon, err := daemonHolder(sock)
	if err != nil {
		return binding.AppServerThread{}
	}
	if loaded, err := codexLoadedThreads(sock); err != nil || !slices.Contains(loaded, thread) {
		return binding.AppServerThread{}
	}
	return binding.AppServerThread{Socket: sock, Thread: thread, Daemon: daemon}
}

// daemonHolder is the identity of the daemon process serving sock.
func daemonHolder(sock string) (launch.HolderIdentity, error) {
	pid, err := codexDaemonPID(sock)
	if err != nil {
		return launch.HolderIdentity{}, err
	}
	return processHolder(pid)
}

// livenessOf is a binding's liveness. A Codex thread's comes from the
// app-server daemon that had it loaded when it was bound: the thread has
// ended only when that same daemon process, still running, no longer has it
// loaded. A restarted or other daemon, or an unreachable one, proves nothing,
// so the thread is unknown (review-1070 P1-B2).
func livenessOf(b binding.Binding) (binding.State, string) {
	state, evidence := b.Liveness()
	if state == binding.Ended || b.AppServer.Thread == "" {
		return state, evidence
	}
	loaded, err := codexLoadedThreads(b.AppServer.Socket)
	if err != nil {
		return binding.Unknown, fmt.Sprintf("app-server unreachable: %v", err)
	}
	if slices.Contains(loaded, b.AppServer.Thread) {
		return binding.Live, "thread is loaded in the app-server"
	}
	recorded := b.AppServer.Daemon
	if recorded.PID <= 0 {
		return binding.Unknown, "thread is not loaded, and no app-server process was recorded to compare"
	}
	if pid, err := codexDaemonPID(b.AppServer.Socket); err != nil || pid != recorded.PID {
		return binding.Unknown, "thread is not loaded in another app-server process than the one it was bound in"
	}
	if st, ev := launch.InspectHolder(recorded); st != launch.LeaseValid {
		return binding.Unknown, "thread is not loaded, and the app-server process is not confirmed: " + ev
	}
	return binding.Ended, "thread is no longer loaded in the app-server it was bound in"
}

// inspectOutsideLock runs livenessOf for each current binding that names a
// Codex thread, before the binding lock is taken, and returns an inspector
// for use under the lock. That inspector reuses those results for a binding
// that is still exactly as inspected and reads every other binding without
// a harness, so no daemon is dialed while the lock is held, and a binding
// that changed meanwhile is unknown.
func inspectOutsideLock() binding.Inspector {
	type result struct {
		b        binding.Binding
		state    binding.State
		evidence string
	}
	inspected := map[string]result{}
	all, _ := binding.List()
	for _, b := range all {
		if b.AppServer.Thread != "" {
			state, evidence := livenessOf(b)
			inspected[b.Name] = result{b, state, evidence}
		}
	}
	return func(b binding.Binding) (binding.State, string) {
		if b.AppServer.Thread == "" {
			return b.Liveness()
		}
		if r, ok := inspected[b.Name]; ok && r.b == b {
			return r.state, r.evidence
		}
		return binding.Unknown, "binding changed after it was inspected"
	}
}

// linkShared reports whether a link share of the root at stateDir names a
// binding of b's own session.
func linkShared(stateDir string, b binding.Binding) bool {
	f, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil {
		// An unreadable manifest may hold a share of this session (a
		// missing one loads empty): keep the target identity rather than
		// move the share's name (review-1070 P2-B3).
		return true
	}
	shared := map[string]bool{}
	for _, l := range f.Links {
		for _, sh := range l.Shares {
			shared[sh.Binding] = true
		}
	}
	all, _ := binding.List()
	for _, o := range all {
		if shared[o.Name] && o.Same(b) {
			return true
		}
	}
	return false
}

// detectHarness finds the harness a mailbox attach runs in: Codex by
// CODEX_THREAD_ID, Claude by the registry entry of an ancestor process. A
// session it cannot detect is still attached, with no harness or holder. It
// is a variable because it runs ps and reads the Claude registry; tests
// replace it.
var detectHarness = func() harnessSession {
	if thread := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")); thread != "" {
		h := harnessSession{Kind: "codex"}
		if sock, err := codexDefaultSocket(); err == nil {
			h.AppServer = loadedThread(sock, thread)
			if h.AppServer.Thread != "" {
				h.Cwd, _ = codexThreadCwd(sock, thread)
			}
		}
		return h
	}
	ancestors, _ := ancestorPIDs(os.Getpid())
	for _, pid := range ancestors {
		if s, err := claudeSession(pid); err == nil {
			return harnessSession{Kind: "claude", PID: pid, Cwd: s.Cwd}
		}
	}
	return harnessSession{}
}

// pruneAfterAttach prunes the bindings of ended sessions once attach has
// written its own, so the write sees an ended binding of its own identity
// and takes its name back (review-s2 P1-1). The attach has succeeded by then,
// so a failed prune is left to the next attach or `bindings`, which reports it.
func pruneAfterAttach() { _, _ = binding.PruneEnded(inspectOutsideLock()) }

// bindingRow is one bound session as `bindings` reports it.
type bindingRow struct {
	Name     string        `json:"name"`
	Label    string        `json:"label,omitempty"`
	State    binding.State `json:"state"`
	Evidence string        `json:"evidence"`
	Root     string        `json:"root"`
	Handle   string        `json:"handle,omitempty"`
	Target   string        `json:"target,omitempty"`
}

// bindings lists the bound sessions with their liveness (/amq-remote
// status), after it prunes the ones proven ended. A session whose liveness is
// unknown is listed and kept.
func bindings(args []string, stdout io.Writer, probe ...*jsonProbe) (int, error) {
	fs := flag.NewFlagSet("bindings", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	inspect := inspectOutsideLock()
	pruned, err := binding.PruneEnded(inspect)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	all, err := binding.List()
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	rows := make([]bindingRow, 0, len(all))
	width := 0
	for _, b := range all {
		state, evidence := inspect(b)
		rows = append(rows, bindingRow{Name: b.Name, Label: b.Label, State: state, Evidence: evidence, Root: b.Root, Handle: b.Handle, Target: b.Target})
		width = max(width, len([]rune(nonEmpty(b.Label, b.Name))))
	}
	if c.json {
		if pruned == nil {
			pruned = []string{}
		}
		emitJSON(stdout, map[string]any{"bindings": rows, "pruned": pruned})
		return 0, nil
	}
	if len(rows) == 0 {
		say(stdout, "No session is connected.")
	}
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-7s  %s", width, nonEmpty(r.Label, r.Name), r.State, r.Name)
		if r.State != binding.Live {
			line += ": " + r.Evidence
		}
		say(stdout, "%s", line)
	}
	if len(pruned) > 0 {
		say(stdout, "Pruned ended sessions: %s.", strings.Join(pruned, ", "))
	}
	return 0, nil
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
	removed, err := binding.Retire(match)
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

// findSelf finds the invoking native session. It is a variable because the
// real search reads the Claude registry and runs ps; tests replace it.
var findSelf = selfCandidate

// claudeStopHookState is the state of the AMQ Stop hook in the user's Claude
// settings. It is a variable so tests never read the real ~/.claude.
var claudeStopHookState = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return claude.StopHookState(home)
}

// requireStopHook refuses a Claude session whose settings lack the AMQ Stop
// hook: a remote task there is admitted and never completes, so it stays
// running and parks every later task as busy (linked server's first live
// run, amq-remote 0.97.0). attach never edits ~/.claude/settings.json; it
// names the command that does.
func requireStopHook() error {
	state, err := claudeStopHookState()
	switch {
	case err != nil:
		return fmt.Errorf("cannot read ~/.claude/settings.json to check the AMQ Stop hook: %v; repair it, then attach again", err)
	case state == claude.StopHookMissing:
		return protocol.Refuse(protocol.CodeUnsupported, "the AMQ Stop hook is not installed, so a remote task in this Claude Code session would never finish; run `amq-remote claude install-stop-hook`, then attach again")
	}
	return nil
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

// ancestorPIDs lists the parent chain of pid, nearest first. It runs ps, so
// it is a variable that tests replace.
var ancestorPIDs = psAncestorPIDs

func psAncestorPIDs(pid int) ([]int, error) {
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
