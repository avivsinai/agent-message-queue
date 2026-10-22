package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	amqcli "github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/activity"
	"github.com/avivsinai/agent-message-queue/internal/remote/amqio"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	"github.com/avivsinai/agent-message-queue/internal/remote/sender"
)

// spoolReapHorizon is the age at which settled sender envelopes are reaped.
// Dispatched/expired/failed entries are removed once the caller has had a
// chance to observe the outcome, bounding the spool's growth.
const spoolReapHorizon = 6 * time.Hour

// serveFlags holds pointers to the flags serve defines beyond the common
// root/json pair. defineServeFlags is the single registration site: serve()
// reads them to run, and up()'s FlagSet registers the same set so serve's
// options can be forwarded by value: two registration sites would panic with
// "flag redefined" on every up invocation.
type serveFlags struct {
	me           *string
	useFake      *bool
	codexSocket  *string
	codexThread  *string
	codexApprove *bool
	manifestPath *string
	discover     *bool
	poll         *time.Duration
}

// defineServeFlags registers serve's flags on fs exactly once and returns
// their pointers. meDefault differs per command: serve defaults the endpoint
// handle to amqio.DefaultHandle, up to its own supervisor default.
func defineServeFlags(fs *flag.FlagSet, meDefault string) serveFlags {
	return serveFlags{
		me:           fs.String("me", meDefault, "endpoint mailbox handle in the root"),
		useFake:      fs.Bool("fake", false, "register the deterministic fake runtime as target 'fake' (sugar: appends to manifest)"),
		codexSocket:  fs.String("codex-socket", "", "unix socket of the running Codex app-server daemon; attaches its loaded threads (sugar: appends to manifest)"),
		codexThread:  fs.String("codex-thread", "", "attach only this Codex thread id (with --codex-socket)"),
		codexApprove: fs.Bool("codex-approve", false, "answer approvals of runs submitted to those Codex threads from remote clients; the first answer wins"),
		manifestPath: fs.String("manifest", "", "path to the adapter manifest (default: <stateDir>/manifest.json)"),
		discover:     fs.Bool("discover", false, "list discovered adapter candidates and exit (attaches nothing)"),
		poll:         fs.Duration("poll", 500*time.Millisecond, "AMQ import and reconciliation interval"),
	}
}

// serve runs the endpoint: store, attachments, IPC socket, AMQ import loop.
func serve(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	sf := defineServeFlags(fs, amqio.DefaultHandle)
	me, useFake, codexSocket, codexThread := sf.me, sf.useFake, sf.codexSocket, sf.codexThread
	codexApprove, manifestPath, discover, poll := sf.codexApprove, sf.manifestPath, sf.discover, sf.poll
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	pi.Version = version // names the extension tag an old pi bridge must install
	stateDir, err := c.stateDir()
	if err != nil {
		return protocol.ExitUsage, err
	}
	manifestFile := manifest.DefaultPath(stateDir)
	if *manifestPath != "" {
		manifestFile = *manifestPath
	}
	// --discover runs before any startup write or ownership check: no
	// EnsureAgent, no store, no socket, so it also works while an endpoint
	// for this root is running.
	if *discover {
		return discoverAndPrint(c.root, stateDir, manifestFile, *codexSocket, stdout, stderr)
	}
	// Register the endpoint's mailbox handle in config.json so other
	// agents in the root can route to it. This preserves every other agent's
	// config; amqio.New stays free of configuration side effects.
	if added, cerr := config.EnsureAgent(c.root, *me); cerr != nil {
		say(stderr, "warning: could not register handle %q in config.json: %v", *me, cerr)
	} else if added {
		say(stderr, "registered handle %q in %s", *me, filepath.Join(c.root, "meta", "config.json"))
	}
	// Carrier publish callback: nil-safe until startupSequence assigns the
	// carrier. SetPublish runs inside startupSequence; this closure forwards
	// to the carrier once it exists.
	var carrier *amqio.Carrier
	// Buzz DM carriers are built below, once the manifest path is
	// known and before startup reconciliation.
	var relayCfg *manifest.Relay
	var edges *dmEdges
	links := newLinkSet(c.root, stateDir, manifestFile, version, stderr)
	carrierPublish := publishRouter(map[string]publishFunc{
		"link": links.publish,
		"amq": func(s protocol.Snapshot, origin map[string]string) error {
			if carrier == nil {
				return errCarrierUnavailable
			}
			return carrier.Publish(s, origin)
		},
		"buzz": func(s protocol.Snapshot, origin map[string]string) error {
			if edges == nil {
				return errCarrierUnavailable
			}
			return edges.publish(s, origin)
		},
	})
	// The manifest load, flag-sugar append (--fake, --codex-socket),
	// validation and owned startup are ONE production path, serveStartupFrom,
	// shared verbatim by the focused regressions. Flag sugar is
	// built here (it needs LoadedThreads/flags) and handed in as entries; a
	// duplicate target is exit 2 via the typed validation error. serveStartupFrom
	// returns Load/Validate failures classified: validation -> exit 2, I/O or
	// parse -> runtime error.
	var sugar []manifest.Adapter
	if *useFake {
		sugar = append(sugar, manifest.Adapter{Kind: "fake", Target: "fake", Epoch: "e_1"})
	}
	if *codexSocket != "" {
		codex.Version = version
		threads := []string{*codexThread}
		if *codexThread == "" {
			threads, err = codex.LoadedThreads(*codexSocket)
			if err != nil {
				return 0, fmt.Errorf("list codex threads: %w", err)
			}
		}
		for _, id := range threads {
			cfg, _ := json.Marshal(struct {
				Socket  string `json:"socket"`
				Thread  string `json:"thread"`
				Approve bool   `json:"approve"`
			}{Socket: *codexSocket, Thread: id, Approve: *codexApprove})
			sugar = append(sugar, manifest.Adapter{Kind: "codex", Target: codex.TargetID(id), Config: cfg})
		}
	}
	// One manifest snapshot drives target attachment, the Buzz DM carriers
	// and the relay alike: a second read could see a different file. Buzz DM
	// carriers exist before startup reconciliation, so a Buzz record owed
	// from before a restart publishes during reconcile.
	mfSnap, err := manifest.Load(manifestFile)
	var ep *core.Endpoint
	if err == nil {
		relayCfg = mfSnap.Relay
		edges = buildDMEdges(c.root, stateDir, relayCfg, stderr)
		edges.pinApprovals(mfSnap.Adapters, stderr)
		defer edges.unpinApprovals()
		_, ep, carrier, _, err = serveStartupFrom(stateDir, c.root, *me, mfSnap, sugar, carrierPublish, &carrier, stderr, wireCarrier(c.root, stderr))
	}
	if err != nil {
		return serveStartupError(err, sugar, manifestFile)
	}
	// carrier.Warn and the cross-project reply router are installed
	// inside startupSequence (wireCarrier) BEFORE Reconcile, so the startup
	// revision already publishes through the shipped handlers.
	// Durable sender: open the outgoing spool and a drainer. The drainer
	// replays pending envelopes (CLI submits that were persisted while the
	// companion was down) through the endpoint on each tick, then reaps
	// settled entries. The spool is separate durable storage from the request
	// store: up supervises this companion but does not own the stored data.
	spool, err := sender.Open(stateDir)
	if err != nil {
		_ = ep.Close()
		return 0, err
	}
	drainer := sender.NewDrainer(spool, ep)
	server, err := ipc.Listen(stateDir, ep)
	if err != nil {
		_ = ep.Close()
		return 0, err
	}
	// Claude targets no relay share serves take Buzz Desktop denies
	// through an owner-less pin: those attached at startup now,
	// those attached later as they register.
	pinNative := func(a manifest.Adapter) { edges.pinNative(a, ep.NativeSessionID(a.Target), stderr) }
	for _, a := range mfSnap.Adapters {
		pinNative(a)
	}
	server.SetRegistrar(liveRegistrar(c.root, stateDir, ep, pinNative))
	server.SetLinkHandler(links.request)
	// amq-acp mirrors a Buzz-started request's native activity into Desktop.
	server.SetActivityHandler(activity.MirrorHandler(ep))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	say(stdout, "amq-remote %s serving root=%s handle=%s socket=%s targets=%d", version, c.root, *me, server.Path(), len(ep.Targets()))
	// The attached targets, printed by the process that attached them, so
	// `up` shows them from its own child and never from some other endpoint
	// answering on the socket.
	for _, s := range ep.Sessions() {
		printSession(stdout, s)
	}
	// Relay surface: one authenticated client per share. The
	// manifest was already loaded and validated by serveStartupFrom; the
	// set follows later manifest edits. Fresh remote commands are
	// admitted only now, after attachment and startup reconciliation, and
	// also for a serve that has no share yet.
	edges.bind(ep.Handle, ep.NativeSessionID)
	edges.bindAttachments(ep.AttachmentOf)
	ep.SetThreadApprovals(edges.bindThreadApprovals(ep.RespondThreadApproval))
	relays := newRelaySet(c.root, stateDir, manifestFile, edges, stderr)
	relays.start(ctx, relayCfg)
	if relayCfg != nil {
		say(stdout, "relay %s: %d shared session(s)", relayCfg.URL, len(relayCfg.Shares))
	}
	ep.SetConsent(links.consentLive)
	links.start(ctx, ep, mfSnap)
	go func() {
		t := time.NewTicker(*poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := carrier.ImportOnce(); err != nil {
					say(stderr, "import: %v", err)
				}
				if err := ep.Tick(); err != nil {
					say(stderr, "tick: %v", err)
				}
				links.tick()
				relays.tick()
				// Replay durable sender envelopes (CLI submits persisted while the
				// companion was down), then reap settled ones older than the reap
				// horizon so the spool is bounded. Reap on every tick,
				// independent of Drain activity — a live submit settles its own
				// envelope so Drain returns zero forever, and Reap must still run.
				if _, derr := drainer.Drain(ctx); derr != nil {
					say(stderr, "drain: %v", derr)
				}
				_, _ = spool.Reap(time.Now().Add(-spoolReapHorizon), 64)
			}
		}
	}()
	err = server.Serve(ctx)
	// Cancel before joining: Serve can also return on an accept error with
	// ctx still live, and the relays would never end.
	stop()
	relays.wait()
	if cerr := ep.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return 0, err
}

// serveStartupError maps a failed manifest load or startup to serve's exit:
// a validation error is exit 2, naming the sources of a duplicate target;
// any other error is a runtime error.
func serveStartupError(err error, sugar []manifest.Adapter, manifestFile string) (int, error) {
	if manifest.IsValidation(err) {
		if dup, ok := err.(*manifest.ErrDuplicateTarget); ok {
			// Name the actual sources: the duplicate may be two
			// identical manifest entries with no flag passed.
			inFlags := false
			for _, adapter := range sugar {
				if adapter.Target == dup.Target {
					inFlags = true
					break
				}
			}
			inManifest := false
			var mfBody manifest.File
			if body, readErr := os.ReadFile(manifestFile); readErr == nil {
				if json.Unmarshal(body, &mfBody) == nil {
					for _, adapter := range mfBody.Adapters {
						if adapter.Target == dup.Target {
							inManifest = true
							break
						}
					}
				}
			}
			switch {
			case inFlags && inManifest:
				return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "duplicate target %q: flags and manifest must not share a target id", dup.Target)
			case inManifest:
				return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "duplicate target %q: manifest declares the same target id more than once", dup.Target)
			default:
				return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "duplicate target %q: the same target id was passed more than once", dup.Target)
			}
		}
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	return 0, err
}

// refusalEntry is the JSON shape of a persisted adapter refusal, shared by
// persistRefusals (write) and loadRefusals (read) and printed by doctor.
type refusalEntry struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Error  string `json:"error"`
}

// persistRefusals writes adapter refusals as typed JSON data in the state
// dir so `amq-remote doctor` can print them. Rewritten EVERY start, including
// an empty list (clears stale refusals). The file is advisory: serve
// continues without the refused adapters; the refusals are not fatal.
func persistRefusals(stateDir string, refusals []registry.Outcome) error {
	entries := make([]refusalEntry, 0, len(refusals))
	for _, r := range refusals {
		entries = append(entries, refusalEntry{
			Kind:   r.Manifest.Kind,
			Target: r.Manifest.Target,
			Error:  r.Refusal.Error(),
		})
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	// Atomic: a truncated refusals.json would make doctor silently drop
	// the refusal and exit 0. Same durability as every other
	// generated-state file.
	_, err = fsq.WriteFileAtomic(stateDir, "refusals.json", data, 0600)
	return err
}

// writeEffectiveAdapters writes the effective adapter set (attached + refused)
// as generated state next to refusals.json. This is NOT the user's manifest —
// it is rewritten each start and never touches the operator's file.
func writeEffectiveAdapters(stateDir string, mf manifest.File, attached []core.Attachment, refusals []registry.Outcome) error {
	type effEntry struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
		State  string `json:"state"`
		Error  string `json:"error,omitempty"`
	}
	entries := make([]effEntry, 0, len(mf.Adapters))
	// Kind per attached target comes from the manifest: it is the
	// authority that published the attachment.
	kinds := make(map[string]string, len(mf.Adapters))
	for _, adapter := range mf.Adapters {
		kinds[adapter.Target] = adapter.Kind
	}
	for _, att := range attached {
		target := att.Inspect().TargetID
		entries = append(entries, effEntry{
			Kind:   kinds[target],
			Target: target,
			State:  "attached",
		})
	}
	for _, r := range refusals {
		entries = append(entries, effEntry{
			Kind:   r.Manifest.Kind,
			Target: r.Manifest.Target,
			State:  "refused",
			Error:  r.Refusal.Error(),
		})
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(stateDir, "adapters.json", data, 0600)
	return err
}

// loadRefusals reads persisted adapter refusals for doctor to print.
func loadRefusals(stateDir string) ([]refusalEntry, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "refusals.json"))
	if err != nil {
		return nil, err
	}
	var entries []refusalEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// wireCarrier installs the shipped durability-warning handler and the
// cross-project reply router on the carrier. serve hands it to
// serveStartupFrom, which runs it inside startupSequence between amqio.New and
// Reconcile, so the startup revision publishes through the fully-configured
// carrier.
func wireCarrier(root string, warn io.Writer) func(*amqio.Carrier) {
	return func(c *amqio.Carrier) {
		if warn != nil {
			c.Warn = func(e error) {
				// Best-effort operator surface: a failed warning write must
				// never take down the endpoint that is already degrading.
				_, _ = fmt.Fprintf(warn, "amq-remote: durability warning: %v\n", e)
			}
		}
		c.SetReplyRouter(replyRouterFor(root))
	}
}

// replyRouterFor returns the ReplyRouter the endpoint uses: cli resolves the
// route, and this adapter translates cli's vocabulary into the carrier's.
// It is a named function, not an inline closure, so a test can exercise the
// SHIPPED translation rather than reimplementing it
// (TestImportCrossProjectRealRouterTransientPeerAbsent fails if the
// ErrPeerRootUnreachable -> TransientRouteError translation is removed).
func replyRouterFor(root string) amqio.ReplyRouter {
	return func(replyProject, replyTo string) (string, string, error) {
		r, h, err := amqcli.ResolveReplyRoute(root, replyProject, replyTo)
		// The adapter translates between the cli vocabulary
		// (ErrPeerRootUnreachable) and the amqio vocabulary
		// (TransientRouteError). The generic cli layer must not import amqio;
		// this named function is the seam that already imports both sides.
		if err != nil && errors.Is(err, amqcli.ErrPeerRootUnreachable) {
			return "", "", amqio.NewTransientRouteError(err)
		}
		return r, h, err
	}
}

// openServeStore builds the store and endpoint with exactly the defaults
// serve uses (DefaultMaxStoreBytes, DefaultCompactHorizon). It is extracted
// from serve so a test can exercise the SHIPPED wiring: deleting the quota or
// horizon wiring from this function must fail
// TestBK4ServeWiringCompactionNonVacuous. Reconcile is NOT called here;
// serve calls it after SetPublish + Register + attachments are wired
// (running it here would mark every running record attachment_lost).
func openServeStore(stateDir string) (*requests.Store, *core.Endpoint, error) {
	store, err := requests.Open(stateDir, requests.WithMaxStoreBytes(protocol.DefaultMaxStoreBytes))
	if err != nil {
		return nil, nil, err
	}
	ep := core.New(core.Config{
		Store:          store,
		CompactHorizon: protocol.DefaultCompactHorizon,
	})
	return store, ep, nil
}

// serveStartupFrom is the ONE production startup path serve runs: startup is
// one callable sequence covering the whole validate->Build->persist->sequence
// chain. It appends the caller's flag-sugar entries to the loaded manifest,
// validates, and owns the
// startup. serve loads the manifest once and hands the snapshot in, so target
// attachment and the relay use one validated file. Validation failures come
// back typed so serve maps them to exit 2.
//
// Order matters: Build runs BEFORE startupSequence, so a refusing adapter is
// a typed outcome, not a lost start; generated diagnostics (refusals.json,
// adapters.json) are published only AFTER startupSequence returns — the
// store open inside it creates the state directory and acquires the owner
// lock, so a losing starter never overwrites the live owner's files and a
// fresh directory cannot break the write. Publish failures are warnings on
// the warn writer: doctor reads what exists, and
// losing startup keeps its own exit code. Refusals are rewritten EVERY owned
// start, including empty — a removed adapter must not persist as 'refused'
// forever. The caller owns the returned endpoint: Close it when done.
func serveStartupFrom(stateDir, root, handle string, mf manifest.File, sugar []manifest.Adapter, publish core.Publisher, carrierOut **amqio.Carrier, warn io.Writer, wire func(*amqio.Carrier)) (*requests.Store, *core.Endpoint, *amqio.Carrier, []registry.Outcome, error) {
	mf.Adapters = append(append([]manifest.Adapter(nil), mf.Adapters...), sugar...)
	if verr := manifest.Validate(mf); verr != nil {
		return nil, nil, nil, nil, verr
	}
	var attachments []core.Attachment
	var refusals []registry.Outcome
	if len(mf.Adapters) > 0 {
		for _, oc := range registry.Build(context.Background(), root, stateDir, mf) {
			if oc.Attachment != nil {
				attachments = append(attachments, oc.Attachment)
			} else {
				refusals = append(refusals, oc)
				if warn != nil {
					say(warn, "warning: adapter %q (kind %q) refused: %v", oc.Manifest.Target, oc.Manifest.Kind, oc.Refusal)
				}
			}
		}
	}
	store, ep, carrier, err := startupSequence(stateDir, root, handle, publish, carrierOut, wire, attachments...)
	if err != nil {
		return store, ep, carrier, refusals, err
	}
	if perr := persistRefusals(stateDir, refusals); perr != nil && warn != nil {
		say(warn, "warning: could not persist adapter refusals: %v", perr)
	}
	if werr := writeEffectiveAdapters(stateDir, mf, attachments, refusals); werr != nil && warn != nil {
		say(warn, "warning: could not write effective adapters: %v", werr)
	}
	return store, ep, carrier, refusals, nil
}

// startupSequence is the construction-plus-reconcile order serve runs, as
// ONE callable sequence so tests can pin it: open store, SetPublish,
// construct the carrier, wire it, Register attachments,
// and ONLY THEN Reconcile. Moving Reconcile before SetPublish/Register marks
// every running record attachment_lost and loses the first reconcile
// revision to a no-op publisher. The carrier is constructed and
// wired INSIDE this sequence so the startup revision reaches
// the fully-configured carrier, not a no-op publisher. carrierOut is assigned
// the carrier before Reconcile, so the caller's publish closure (which
// captures the same pointer) sees it during Reconcile. The returned store is
// closed by the caller on error.
func startupSequence(stateDir, root, handle string, publish core.Publisher, carrierOut **amqio.Carrier, wire func(*amqio.Carrier), attachments ...core.Attachment) (*requests.Store, *core.Endpoint, *amqio.Carrier, error) {
	store, ep, err := openServeStore(stateDir)
	if err != nil {
		return nil, nil, nil, err
	}
	// SetPublish wires the endpoint's publisher. In serve, this forwards to
	// the carrier; in tests, a custom publish tracks calls. The carrier is
	// assigned next so Reconcile's publish calls reach it.
	ep.SetPublish(publish)
	// Carrier construction sits between SetPublish and Register. The carrier
	// is assigned before Reconcile so the startup revision reaches it, not a
	// no-op publisher. carrierOut lets the caller's publish closure see the
	// carrier during Reconcile.
	carrier, err := amqio.New(root, handle, ep)
	if err != nil {
		_ = ep.Close()
		return store, nil, nil, err
	}
	// Wire the shipped warn handler + reply router BEFORE Reconcile so
	// the startup revision already publishes through the fully-configured
	// carrier.
	wire(carrier)
	if carrierOut != nil {
		*carrierOut = carrier
	}
	for _, att := range attachments {
		ep.Register(att)
	}
	if err := ep.Reconcile(); err != nil {
		_ = ep.Close()
		return store, nil, nil, fmt.Errorf("reconcile: %w", err)
	}
	return store, ep, carrier, nil
}

// printCandidates prints one discovered candidate per line: kind, target,
// display name, then a manifest entry to paste, and the native session id
// a share entry pins when the discoverer read it. Discovery never attaches:
// registration stays an explicit manifest edit, the owner's
// consent to share the session.
func printCandidates(w io.Writer, cands []registry.Candidate) {
	for _, cand := range cands {
		entry := map[string]any{"kind": cand.Kind, "target": cand.Target}
		if len(cand.Config) > 0 {
			entry["config"] = cand.Config
		}
		raw, _ := json.Marshal(entry)
		display := cand.Display
		if display == "" {
			display = "-"
		}
		if cand.NativeSession != "" {
			say(w, "%-8s %-40s %-20s %s native_session_id=%s", cand.Kind, cand.Target, display, raw, cand.NativeSession)
			continue
		}
		say(w, "%-8s %-40s %-20s %s", cand.Kind, cand.Target, display, raw)
	}
}

// discoverAndPrint lists discovered candidates and exits. It runs before any
// startup write or ownership check: no EnsureAgent, no store, no socket, no
// lifetime lock, so it also works while
// an endpoint or supervisor for this root is running. The manifest classifies
// exactly like the serve path: validation failures are usage errors (exit
// 2), I/O and parse failures are not. An explicit --codex-socket replaces
// the default daemon socket.
func discoverAndPrint(root, stateDir, manifestFile, codexSocket string, stdout, stderr io.Writer) (int, error) {
	mf, lerr := manifest.Load(manifestFile)
	if lerr != nil {
		if manifest.IsValidation(lerr) {
			return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", lerr)
		}
		return 0, lerr
	}
	if verr := manifest.Validate(mf); verr != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", verr)
	}
	cands, diags := registry.Discover(context.Background(), registry.DiscoverRequest{
		Root: root, StateDir: stateDir,
		Hints: map[string]string{codex.HintSocket: codexSocket},
	})
	printCandidates(stdout, cands)
	// Each failed or incomplete discoverer is reported on stderr; the other
	// discoverers' candidates above stand. An explicitly supplied unusable
	// hint is a usage error.
	for _, d := range diags {
		if errors.Is(d.Err, registry.ErrBadHint) {
			return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "discover %s: %v", d.Kind, d.Err)
		}
	}
	for _, d := range diags {
		say(stderr, "amq-remote: discover %s: %v", d.Kind, d.Err)
	}
	return 0, nil
}
