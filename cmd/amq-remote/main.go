// Command amq-remote is the AMQ Remote companion: an endpoint that attaches to
// running harness sessions and a CLI that submits, follows, and cancels
// requests against it. It is a companion beside amq, like amq-bridge and
// amq-keepalive; amq itself gains no socket. See docs/adr-remote-control.md.
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
	"strings"
	"syscall"
	"time"

	amqcli "github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/remote/amqio"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	_ "github.com/avivsinai/agent-message-queue/internal/remote/fake" // registers fake factory
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	"github.com/avivsinai/agent-message-queue/internal/remote/sender"
)

var version = "dev"

// stateDirName is the extension directory under the AMQ root that the
// endpoint owns. amq cleanup never touches extension data.
const stateDirName = "extensions/remote"

// spoolReapHorizon is the age at which settled sender envelopes are reaped.
// Dispatched/expired/failed entries are removed once the caller has had a
// chance to observe the outcome, bounding the spool's growth.
const spoolReapHorizon = 6 * time.Hour

const usageText = `Usage: amq-remote <command> [options]

Attach to a running harness session from another client. A request has an
identity and a result; every reply is a snapshot of that request.

Commands:
  serve                    Run the endpoint for this root (foreground)
  sessions                 List shared runtimes and their capabilities
  inspect TARGET           Current state of one runtime
  submit TARGET            Submit one work request; prints its receipt
  status REQUEST_REF       Current recorded snapshot
  wait REQUEST_REF         Wait for that request's outcome
  cancel REQUEST_REF       Cancel that exact request
  requests                 Recent local request records
  doctor                   Diagnose the endpoint chain
  version                  Print the version

Common options:
  --root DIR    AMQ root (default: AM_ROOT)
  --json        Machine-readable output on stdout; diagnostics on stderr

Exit codes: 0 ok · 1 work failed or cancelled · 2 usage · 3 not found ·
4 timeout · 5 context mismatch · 6 action required (busy, unsupported,
unshared, expired, uncertain) · 130 interrupted
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type common struct {
	root string
	json bool
}

func addCommon(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.root, "root", os.Getenv("AM_ROOT"), "AMQ root directory (default AM_ROOT)")
	fs.BoolVar(&c.json, "json", false, "emit JSON")
	return c
}

func (c *common) stateDir() (string, error) {
	if c.root == "" {
		return "", protocol.Refuse(protocol.CodeInvalid, "--root or AM_ROOT is required")
	}
	if !filepath.IsAbs(c.root) {
		return "", protocol.Refuse(protocol.CodeInvalid, "--root must be absolute")
	}
	return filepath.Join(c.root, stateDirName), nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		say(stderr, "%s", usageText)
		return protocol.ExitUsage
	}
	switch args[0] {
	case "-v", "--version", "version":
		say(stdout, "amq-remote %s\n", version)
		return 0
	}
	cmd, rest := args[0], args[1:]
	var err error
	var out any
	var code int
	switch cmd {
	case "serve":
		code, err = serve(rest, stdout, stderr)
		return finish(stderr, nil, false, code, err)
	case "sessions":
		out, code, err = clientSimple(rest, protocol.OpSessionList, "")
	case "inspect":
		out, code, err = clientSimple(rest, protocol.OpSessionInspect, "TARGET")
	case "submit":
		out, code, err = submit(rest, stdin)
	case "status":
		out, code, err = status(rest)
	case "wait":
		out, code, err = wait(rest)
	case "cancel":
		out, code, err = cancel(rest)
	case "requests":
		out, code, err = listRequests(rest)
	case "doctor":
		out, code, err = doctor(rest)
	default:
		say(stderr, "unknown command %q\n%s", cmd, usageText)
		return protocol.ExitUsage
	}
	wantJSON := hasFlag(rest, "json")
	return finish(stdout, out, wantJSON, code, err)
}

// parseInterleaved accepts flags before or after positional arguments, the
// way the other amq companions do, and returns the positionals in order.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name || a == "-"+name || strings.HasPrefix(a, "--"+name+"=") {
			return true
		}
	}
	return false
}

// finish prints the reply and maps the outcome to the AMQ exit contract.
func finish(w io.Writer, out any, asJSON bool, code int, err error) int {
	if err != nil {
		if asJSON {
			body := map[string]any{"error": err.Error()}
			var r *protocol.Refusal
			if errors.As(err, &r) {
				body["code"] = string(r.Code)
			}
			_ = json.NewEncoder(w).Encode(body)
		} else {
			fmt.Fprintln(os.Stderr, "amq-remote:", err)
		}
		if code != 0 {
			return code
		}
		return protocol.ExitCode(err)
	}
	if out != nil {
		if asJSON {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
		} else {
			printHuman(w, out)
		}
	}
	return code
}

func printHuman(w io.Writer, out any) {
	switch v := out.(type) {
	case sender.SpoolReceipt:
		// B3: the sender-side receipt is NOT a Snapshot — it has no request
		// ref or revision. Print it as its own shape.
		say(w, "submitted  %s  (pending, %s)", v.RequestID, v.Reason)
		if v.TargetID != "" {
			say(w, "  target=%s", v.TargetID)
		}
		say(w, "\n")
	case protocol.Reply:
		printHuman(w, v.Snapshot)
		if v.Outcome.Code != "" {
			say(w, "outcome=%s\n", v.Outcome.Code)
		}
		if v.Outcome.Evidence != "" {
			say(w, "evidence=%s\n", v.Outcome.Evidence)
		}
		if v.Outcome.Disposition != "" {
			say(w, "cancel=%s\n", v.Outcome.Disposition)
		}
	case protocol.Snapshot:
		say(w, "%s  %s", v.State, v.RequestRef)
		if v.Code != "" {
			say(w, "  code=%s", v.Code)
		}
		if v.NativeRun != nil {
			say(w, "  run=%s", *v.NativeRun)
		}
		say(w, "\n")
		if v.Result != nil {
			say(w, "%s\n", v.Result.Text)
			if v.Result.Truncated {
				say(w, "%s\n", "[truncated; full result stays with the harness]")
			}
		}
	case []protocol.Session:
		for _, s := range v {
			printSession(w, s)
		}
	case protocol.Session:
		printSession(w, v)
	default:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	}
}

func printSession(w io.Writer, s protocol.Session) {
	caps := []string{}
	if s.Capabilities.Submit {
		caps = append(caps, "submit")
	}
	if s.Capabilities.CancelRequest {
		caps = append(caps, "cancel")
	}
	if s.Capabilities.Steer {
		caps = append(caps, "steer")
	}
	if s.Capabilities.ApproveTool {
		caps = append(caps, "approve")
	}
	if s.Capabilities.AnswerQuestion {
		caps = append(caps, "answer")
	}
	say(w, "%-24s %-8s %-8s %-8s epoch=%s caps=%s\n", s.TargetID, s.Harness, s.Attachment, s.Status, s.Epoch, strings.Join(caps, ","))
}

// serve runs the endpoint: store, attachments, IPC socket, AMQ import loop.
func serve(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	me := fs.String("me", amqio.DefaultHandle, "endpoint mailbox handle in the root")
	useFake := fs.Bool("fake", false, "register the deterministic fake runtime as target 'fake' (sugar: appends to manifest)")
	codexSocket := fs.String("codex-socket", "", "unix socket of the running Codex app-server daemon; attaches its loaded threads (sugar: appends to manifest)")
	codexThread := fs.String("codex-thread", "", "attach only this Codex thread id (with --codex-socket)")
	codexApprove := fs.Bool("codex-approve", false, "advertise approve_tool for Codex threads (only after approval fanout is verified live)")
	manifestPath := fs.String("manifest", "", "path to the adapter manifest (default: <stateDir>/manifest.json)")
	discover := fs.Bool("discover", false, "list discovered adapter candidates and exit (attaches nothing)")
	poll := fs.Duration("poll", 500*time.Millisecond, "AMQ import and reconciliation interval")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return protocol.ExitUsage, err
	}
	// .10: register the endpoint's mailbox handle in config.json so other
	// agents in the root can route to it. This preserves every other agent's
	// config; amqio.New stays free of configuration side effects.
	if added, cerr := config.EnsureAgent(c.root, *me); cerr != nil {
		say(stderr, "warning: could not register handle %q in config.json: %v\n", *me, cerr)
	} else if added {
		say(stderr, "registered handle %q in %s\n", *me, filepath.Join(c.root, "meta", "config.json"))
	}
	// Carrier publish callback: nil-safe until startupSequence assigns the
	// carrier. SetPublish runs inside startupSequence; this closure forwards
	// to the carrier once it exists.
	var carrier *amqio.Carrier
	carrierPublish := func(s protocol.Snapshot, origin map[string]string) error {
		if carrier == nil {
			return nil
		}
		return carrier.Publish(s, origin)
	}
	// .13: load the adapter manifest (extensions/remote/manifest.json) and
	// build attachments via the capability-driven registry. Flags (--fake,
	// --codex-socket) APPEND to the manifest set; a duplicate target is exit 2.
	// The manifest is the single source of truth for what runs; serve is never
	// edited for a new adapter (registry.Register in init).
	manifestFile := manifest.DefaultPath(stateDir)
	if *manifestPath != "" {
		manifestFile = *manifestPath
	}
	mf, err := manifest.Load(manifestFile)
	if err != nil {
		return 0, err
	}
	// --discover lists candidates from registered discoverers and exits.
	if *discover {
		cands, derr := registry.Discover(context.Background(), c.root, stateDir)
		if derr != nil {
			return 0, derr
		}
		for _, cand := range cands {
			say(stdout, "%-12s %s\n", cand.Kind, cand.Target)
		}
		return 0, nil
	}
	// Flags append to the manifest set (sugar, back-compat).
	if *useFake {
		mf.Adapters = append(mf.Adapters, manifest.Adapter{Kind: "fake", Target: "fake", Epoch: "e_1"})
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
			mf.Adapters = append(mf.Adapters, manifest.Adapter{Kind: "codex", Target: id, Config: cfg})
		}
	}
	// Re-validate after flag append: a target present in both is exit 2.
	if verr := manifest.Validate(mf); verr != nil {
		if dup, ok := verr.(*manifest.ErrDuplicateTarget); ok {
			return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "duplicate target %q: flags and manifest must not share a target id", dup.Target)
		}
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", verr)
	}
	// Build attachments from the merged manifest via the registry.
	var attachments []core.Attachment
	if len(mf.Adapters) > 0 {
		attachments, err = registry.Build(context.Background(), c.root, stateDir, mf)
		if err != nil {
			// A claude stub refusal is doctor-visible; log and continue without it.
			if errors.Is(err, claude.ErrNotAuthorized) {
				say(stderr, "warning: %v\n", err)
				attachments = nil
			} else {
				return 0, err
			}
		}
	}
	_, ep, carrier, err := startupSequence(stateDir, c.root, *me, nil, carrierPublish, &carrier, attachments...)
	if err != nil {
		return 0, err
	}
	carrier.Warn = func(e error) {
		fmt.Fprintf(os.Stderr, "amq-remote: durability warning: %v\n", e)
	}
	// A cross-project caller must be answered in ITS root, not ours. The
	// carrier holds only the contract; .amqrc discovery, the peer map and
	// session layout stay in the package that owns them.
	root := c.root
	carrier.SetReplyRouter(replyRouterFor(root))
	// Durable sender (.7): open the outgoing spool and a drainer. The drainer
	// replays pending envelopes (CLI submits that were persisted while the
	// companion was down) through the endpoint on each tick, then reaps
	// settled entries. The spool is separate durable storage from the request
	// store: up supervises this companion but does not own the stored data.
	spool, err := sender.Open(stateDir)
	if err != nil {
		_ = ep.Close()
		return 0, err
	}
	drainer := sender.NewDrainer(spool, ep, nil)
	server, err := ipc.Listen(stateDir, ep)
	if err != nil {
		_ = ep.Close()
		return 0, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	say(stdout, "amq-remote %s serving root=%s handle=%s socket=%s targets=%d\n", version, c.root, *me, server.Path(), len(ep.Targets()))
	go func() {
		t := time.NewTicker(*poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := carrier.ImportOnce(); err != nil {
					say(stderr, "import: %v\n", err)
				}
				if err := ep.Tick(); err != nil {
					say(stderr, "tick: %v\n", err)
				}
				// Replay durable sender envelopes (CLI submits persisted while the
				// companion was down), then reap settled ones older than the reap
				// horizon so the spool is bounded. B4: Reap on every tick,
				// independent of Drain activity — a live submit settles its own
				// envelope so Drain returns zero forever, and Reap must still run.
				if _, derr := drainer.Drain(ctx); derr != nil {
					say(stderr, "drain: %v\n", derr)
				}
				_, _ = spool.Reap(time.Now().Add(-spoolReapHorizon), 64)
			}
		}
	}()
	err = server.Serve(ctx)
	if cerr := ep.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return 0, err
}

func clientSimple(args []string, op protocol.Op, positional string) (any, int, error) {
	fs := flag.NewFlagSet(string(op), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: op}
	if positional != "" {
		if len(pos) != 1 {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%s is required", positional)
		}
		cmd.TargetID = pos[0]
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	resp, err := ipc.Call(stateDir, ipc.Request{Command: cmd})
	if err != nil {
		return nil, 0, err
	}
	if err := resp.AsError(); err != nil {
		return nil, 0, err
	}
	if op == protocol.OpSessionList {
		var out []protocol.Session
		if err := json.Unmarshal(resp.Reply, &out); err != nil {
			return nil, 0, err
		}
		return out, 0, nil
	}
	var out protocol.Session
	if err := json.Unmarshal(resp.Reply, &out); err != nil {
		return nil, 0, err
	}
	return out, 0, nil
}

func submit(args []string, stdin io.Reader) (any, int, error) {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	text := fs.String("text", "", "prompt text")
	textFile := fs.String("text-file", "", "read prompt text from a file")
	useStdin := fs.Bool("stdin", false, "read prompt text from stdin")
	busy := fs.String("busy", string(protocol.BusyReject), "reject or queue when the runtime is busy")
	deliver := fs.String("deliver", string(protocol.DeliverTurn), "turn or steer")
	requestID := fs.String("request-id", "", "caller-supplied UUID so a retry reconciles instead of resubmitting")
	window := fs.Duration("admit-within", 2*time.Minute, "latest admission time relative to now (max 24h)")
	minEvidence := fs.String("min-evidence", "", "minimum submit evidence class to require (admitted or submitted; omitted = legacy)")
	epochFlag := fs.String("epoch", "", "registration epoch (offline enqueue: previously-verified epoch; skips live inspect)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "TARGET is required")
	}
	sources := 0
	for _, set := range []bool{*text != "", *textFile != "", *useStdin} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "exactly one of --text, --text-file, --stdin is required")
	}
	body := *text
	switch {
	case *textFile != "":
		data, err := os.ReadFile(*textFile)
		if err != nil {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "read --text-file: %v", err)
		}
		body = string(data)
	case *useStdin:
		data, err := io.ReadAll(io.LimitReader(stdin, protocol.MaxInputBytes+1))
		if err != nil {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "read stdin: %v", err)
		}
		body = string(data)
	}
	if strings.TrimSpace(body) == "" {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "prompt is empty")
	}
	if *window <= 0 || *window > 24*time.Hour {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--admit-within must be within (0, 24h]")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	target := pos[0]
	// Resolve target+epoch. A live inspect is the default; --epoch supplies a
	// previously-verified epoch for offline enqueue (the companion is not
	// running, or the target was inspected earlier). The spool never
	// retargets: it stores exactly the epoch the caller supplied, so an
	// offline enqueue cannot silently bind to a different session.
	epoch := *epochFlag
	if epoch == "" {
		session, err := inspectTarget(stateDir, target)
		if err != nil {
			return nil, 0, err
		}
		epoch = session.Epoch
	}
	id := *requestID
	if id == "" {
		id, err = newUUID()
		if err != nil {
			return nil, 0, err
		}
	}
	cmd := &protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: id,
		TargetID:  target,
		Epoch:     epoch,
		NotAfter:  protocol.FormatTime(time.Now().Add(*window)),
		Input:     &protocol.SubmitInput{Text: body, Busy: protocol.Busy(*busy), Deliver: protocol.Deliver(*deliver), MinEvidence: *minEvidence},
	}
	// Durable sender (.7): persist the exact command, identity, destination,
	// target, epoch and expiry atomically BEFORE returning submitted. If the
	// endpoint is unreachable or crashes mid-dispatch, the companion's drainer
	// replays this envelope after restart with the same identity and bytes.
	spool, err := sender.Open(stateDir)
	if err != nil {
		return nil, 0, err
	}
	env := &sender.Envelope{
		RequestID:   id,
		CreatorHost: ipc.LocalHost,
		TargetID:    target,
		Epoch:       epoch,
		NotAfter:    cmd.NotAfter,
		Command:     cmd,
		Destination: "ipc:" + stateDir,
	}
	if err := spool.Create(env); err != nil {
		return nil, 0, err
	}
	// Attempt live dispatch. If the endpoint is up, return its reply and mark
	// the envelope dispatched. If it is unreachable, return a `submitted`
	// receipt from the spool: the intent is durable and the drainer will
	// dispatch it when the companion runs.
	rep, derr := callReply(stateDir, cmd)
	if derr != nil {
		// A duplicate (request_conflict) means a prior submit for this id
		// already reached the endpoint; surface the stored reply instead of
		// a spool receipt.
		if isDuplicateConflict(derr) {
			// B3: the receipt is NOT a Snapshot — it never mints a request ref
			// or revision the endpoint did not assign.
			return env.Receipt("duplicate_conflict"), exitForState(protocol.StateDispatching), nil
		}
		if isEndpointUnreachable(derr) {
			// The intent is persisted; the caller is told `submitted`, not
			// an error. The drainer replays after restart. B3: the receipt is a
			// sender-side SpoolReceipt, not a fabricated Snapshot.
			return env.Receipt("endpoint_unreachable"), protocol.ExitSuccess, nil
		}
		return nil, 0, derr
	}
	// B2: the endpoint returns refusals (stale_epoch, expired, unsupported,
	// etc.) as a Reply with Outcome.Code and nil error. A non-empty code
	// means the endpoint REFUSED the command — the envelope must NOT be
	// marked dispatched. Mark it failed (terminal) so the caller learns the
	// truth via status; a transient code (busy) is left pending for the
	// drainer to retry.
	if rep.Outcome.Code != "" {
		code := rep.Outcome.Code
		switch code {
		case protocol.CodeBusy, protocol.CodeDraining, protocol.CodeStorageFull:
			// Transient: leave pending, drainer retries next tick.
			_ = spool.MarkAttempt(sender.Key{CreatorHost: ipc.LocalHost, RequestID: id}, string(code))
		default:
			// Terminal refusal: mark failed with the code.
			_ = spool.MarkFailed(sender.Key{CreatorHost: ipc.LocalHost, RequestID: id}, string(code))
		}
		return rep, exitForOutcome(rep), nil
	}
	_ = spool.MarkDispatched(sender.Key{CreatorHost: ipc.LocalHost, RequestID: id})
	return rep, exitForOutcome(rep), nil
}

func inspectTarget(stateDir, target string) (protocol.Session, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: target}})
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

func callReply(stateDir string, cmd *protocol.Command) (protocol.Reply, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Command: cmd})
	if err != nil {
		return protocol.Reply{}, err
	}
	if err := resp.AsError(); err != nil {
		return protocol.Reply{}, err
	}
	var rep protocol.Reply
	if err := json.Unmarshal(resp.Reply, &rep); err != nil {
		return protocol.Reply{}, err
	}
	return rep, nil
}

// exitForOutcome maps a request Reply to the exit code. An op-specific
// Outcome.Code (request_conflict, already_resolved) is action-required and must
// not exit 0 just because the snapshot state reads running/completed; otherwise
// the exit follows the recorded state.
func exitForOutcome(rep protocol.Reply) int {
	if rep.Outcome.Code != "" {
		return protocol.ExitCode(&protocol.Refusal{Code: rep.Outcome.Code})
	}
	return exitForReply(rep.Snapshot, false)
}

// exitForReply maps a snapshot to the exit code. submit and status report the
// recorded state: a still-running request is success for them, a rejected or
// uncertain one is action required. wait uses ExitForState directly.
func exitForReply(snap protocol.Snapshot, waiting bool) int {
	if waiting {
		return protocol.ExitForState(snap.State)
	}
	switch snap.State {
	case protocol.StateRejected, protocol.StateUncertain:
		return protocol.ExitActionRequired
	case protocol.StateFailed, protocol.StateCancelled:
		return protocol.ExitError
	}
	return protocol.ExitSuccess
}

func status(args []string) (any, int, error) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	pos, perr := parseInterleaved(fs, args)
	if perr != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "REQUEST_REF is required")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	// Round-4 B6 gap 3: if the positional is a bare request ID (not a ref),
	// resolve the sender spool FIRST. A running endpoint refuses a bare id
	// as invalid, so the spool surface that reports drain failures is
	// unreachable at exactly the moment the failure exists. When the spool
	// has the envelope, use its stored identity; if the spool's envelope is
	// terminal (failed/expired), return it directly without hitting the
	// endpoint. For a real ref, call the endpoint as before.
	refOrID := pos[0]
	if !strings.HasPrefix(refOrID, protocol.RefPrefix) {
		if spoolReceipt, ok := lookupSpoolStatus(stateDir, refOrID); ok {
			if spoolReceipt.State == sender.StateFailed || spoolReceipt.State == sender.StateExpired {
				return spoolReceipt, exitForSpoolReceipt(spoolReceipt), nil
			}
			// Pending/dispatched: use the spool's stored identity for the
			// live endpoint call so the caller sees the endpoint's current
			// state, not just the spool's snapshot.
			refOrID = protocol.EncodeRef(spoolReceipt.CreatorHost, spoolReceipt.TargetID, spoolReceipt.RequestID)
		}
	}
	rep, err := callReply(stateDir, &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: refOrID})
	if err != nil {
		// B6: if the endpoint is unreachable or the record is not found,
		// consult the sender spool. A submit persisted while the companion
		// was down has no request-store record; status must still tell the
		// caller the truth (pending/dispatched/expired/failed).
		if isEndpointUnreachable(err) || isNotFound(err) || isInvalid(err) {
			if spoolReceipt, ok := lookupSpoolStatus(stateDir, pos[0]); ok {
				return spoolReceipt, exitForSpoolReceipt(spoolReceipt), nil
			}
		}
		return nil, 0, err
	}
	return rep, exitForOutcome(rep), nil
}

func wait(args []string) (any, int, error) {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	timeout := fs.Duration("timeout", 0, "give up after this long; the work continues (0 = no limit)")
	pos, perr := parseInterleaved(fs, args)
	if perr != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "REQUEST_REF is required")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	type outcome struct {
		resp *ipc.Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := ipc.Call(stateDir, ipc.Request{Wait: &ipc.WaitRequest{RequestRef: pos[0], TimeoutMS: timeout.Milliseconds()}})
		done <- outcome{resp, err}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	select {
	case <-sig:
		return nil, protocol.ExitInterrupted, protocol.Refuse(protocol.CodeUnsupported, "wait interrupted; the request keeps running")
	case o := <-done:
		if o.err != nil {
			return nil, 0, o.err
		}
		if err := o.resp.AsError(); err != nil {
			return nil, 0, err
		}
		var snap protocol.Snapshot
		if err := json.Unmarshal(o.resp.Reply, &snap); err != nil {
			return nil, 0, err
		}
		if o.resp.TimedOut {
			return snap, protocol.ExitTimeout, nil
		}
		return snap, exitForReply(snap, true), nil
	}
}

func cancel(args []string) (any, int, error) {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	pos, perr := parseInterleaved(fs, args)
	if perr != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "REQUEST_REF is required")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	ref := pos[0]
	_, targetID, _, err := protocol.DecodeRef(ref)
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	session, err := inspectTarget(stateDir, targetID)
	if err != nil {
		return nil, 0, err
	}
	// B6: use the STORED request's epoch, not the current session epoch.
	// A fresh attachment generates a fresh epoch; using it to cancel a
	// request stored under an older epoch is rejected with stale_epoch.
	// Fetch the stored request to get the original epoch.
	epoch := session.Epoch
	getRep, gerr := callReply(stateDir, &protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestGet,
		RequestRef: ref,
	})
	if gerr != nil {
		return nil, 0, gerr
	}
	if getRep.Snapshot.Epoch != "" {
		epoch = getRep.Snapshot.Epoch
	}
	rep, err := callReply(stateDir, &protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestCancel,
		RequestRef: ref,
		TargetID:   targetID,
		Epoch:      epoch,
		NotAfter:   protocol.FormatTime(time.Now().Add(2 * time.Minute)),
	})
	if err != nil {
		return nil, 0, err
	}
	return rep, exitForOutcome(rep), nil
}

func listRequests(args []string) (any, int, error) {
	fs := flag.NewFlagSet("requests", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	limit := fs.Int("limit", 20, "most recent records to show")
	if err := fs.Parse(args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	store, err := requests.OpenReadOnly(stateDir)
	if err != nil {
		return nil, 0, err
	}
	recs, err := store.List()
	if err != nil {
		return nil, 0, err
	}
	if *limit > 0 && len(recs) > *limit {
		recs = recs[len(recs)-*limit:]
	}
	out := make([]protocol.Snapshot, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Snapshot)
	}
	// B6: include ALL spool envelopes (pending, failed, expired, dispatched)
	// that have no request-store record yet, with their actual terminal state
	// and last_error. Submit persisted while the companion was down; the
	// caller must see failures, not just pending. Dedup against existing
	// records so the same request doesn't appear twice.
	if spool, err := sender.Open(stateDir); err == nil {
		if envs, err := spool.List(); err == nil {
			seen := make(map[string]bool, len(recs))
			for _, r := range recs {
				seen[r.RequestID] = true
			}
			for _, env := range envs {
				if seen[env.RequestID] {
					continue
				}
				state := protocol.StateReceived
				switch env.State {
				case sender.StateFailed:
					state = protocol.StateFailed
				case sender.StateDispatched:
					state = protocol.StateDispatching
				case sender.StateExpired:
					state = protocol.StateRejected
				}
				snap := protocol.Snapshot{
					Schema:      "sender_submitted",
					RequestID:   env.RequestID,
					CreatorHost: env.CreatorHost,
					TargetID:    env.TargetID,
					Epoch:       env.Epoch,
					State:       state,
					InputDigest: env.InputDigest,
					NotAfter:    env.NotAfter,
					ObservedAt:  env.CreatedAt,
				}
				if env.LastError != "" {
					snap.Code = protocol.Code(env.LastError)
				}
				if env.State == sender.StateExpired && snap.Code == "" {
					snap.Code = protocol.CodeExpired
				}
				out = append(out, snap)
			}
		}
	}
	return out, 0, nil
}

func doctor(args []string) (any, int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	report := map[string]any{"root": c.root, "state_dir": stateDir, "socket": ipc.SocketPath(stateDir)}
	code := 0
	if _, err := os.Stat(stateDir); err != nil {
		report["endpoint"] = "no state directory; run `amq-remote serve` once"
		return report, protocol.ExitActionRequired, nil
	}
	resp, err := ipc.Call(stateDir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionList}})
	switch {
	case err != nil:
		report["endpoint"] = "not reachable: " + err.Error()
		code = protocol.ExitActionRequired
	case resp.AsError() != nil:
		report["endpoint"] = "refused session.list: " + resp.AsError().Error()
		code = protocol.ExitActionRequired
	default:
		var sessions []protocol.Session
		_ = json.Unmarshal(resp.Reply, &sessions)
		report["endpoint"] = "reachable"
		report["targets"] = len(sessions)
	}
	if store, err := requests.OpenReadOnly(stateDir); err == nil {
		if recs, err := store.List(); err == nil {
			counts := map[string]int{}
			for _, r := range recs {
				counts[string(r.State)]++
			}
			report["records"] = counts
		}
	}
	return report, code, nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(randReader, b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// replyRouterFor returns the ReplyRouter the endpoint uses: cli resolves the
// route, and this adapter translates cli's vocabulary into the carrier's.
// It is a named function, not an inline closure, so a test can exercise the
// SHIPPED translation rather than reimplementing it (B1: deleting the
// ErrPeerRootUnreachable -> TransientRouteError translation from this
// function must fail TestImportCrossProjectRealRouterTransientPeerAbsent).
func replyRouterFor(root string) amqio.ReplyRouter {
	return func(replyProject, replyTo string) (string, string, error) {
		r, h, err := amqcli.ResolveReplyRoute(root, replyProject, replyTo)
		// B1: the adapter translates between the cli vocabulary
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
// from serve so a test can exercise the SHIPPED wiring (round-3 NEW 1):
// deleting the quota or horizon wiring from this function must fail
// TestBK4ServeWiringCompactionNonVacuous. Reconcile is NOT called here;
// serve calls it after SetPublish + Register + attachments are wired
// (round-4 P0: running it here marks every running record attachment_lost).
func openServeStore(stateDir string, now func() time.Time) (*requests.Store, *core.Endpoint, error) {
	store, err := requests.Open(stateDir, requests.WithMaxStoreBytes(protocol.DefaultMaxStoreBytes))
	if err != nil {
		return nil, nil, err
	}
	cfg := core.Config{
		Store:          store,
		CompactHorizon: protocol.DefaultCompactHorizon,
	}
	if now != nil {
		cfg.Now = now
	}
	ep := core.New(cfg)
	return store, ep, nil
}

// startupSequence is the construction-plus-reconcile order serve runs, as
// ONE callable sequence so tests can pin it (611.22.19 round-4 P0): open
// store, SetPublish, carrier publish callback, Register attachments, and
// ONLY THEN Reconcile. Moving Reconcile before SetPublish/Register marks
// every running record attachment_lost and loses the first reconcile
// revision to a no-op publisher — both round-5 P0 regressions in main_test.go
// go red on exactly that inversion. The carrier is constructed INSIDE this
// sequence (round-6: store, SetPublish, carrier, Register, Reconcile) so the
// carrier is assigned before Reconcile runs — the startup revision reaches
// the carrier, not a no-op publisher. The carrierOut parameter (if non-nil)
// is assigned the carrier before Reconcile, so the caller's publish closure
// (which captures the same carrier pointer) sees it during Reconcile. serve
// passes its attachments; the returned store is closed by the caller on error.
func startupSequence(stateDir, root, handle string, now func() time.Time, publish core.Publisher, carrierOut **amqio.Carrier, attachments ...core.Attachment) (*requests.Store, *core.Endpoint, *amqio.Carrier, error) {
	store, ep, err := openServeStore(stateDir, now)
	if err != nil {
		return nil, nil, nil, err
	}
	// SetPublish wires the endpoint's publisher. In serve, this forwards to
	// the carrier; in tests, a custom publish tracks calls. The carrier is
	// assigned next so Reconcile's publish calls reach it.
	ep.SetPublish(publish)
	// Carrier construction (round-6: inside startupSequence, between SetPublish
	// and Register). The carrier is assigned before Reconcile so the startup
	// revision reaches it, not a no-op publisher. carrierOut lets the caller's
	// publish closure see the carrier during Reconcile.
	carrier, err := amqio.New(root, handle, ep)
	if err != nil {
		_ = ep.Close()
		return store, nil, nil, err
	}
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

// isDuplicateConflict reports whether err is a request_conflict: a prior
// submit for this request id already reached the endpoint.
func isDuplicateConflict(err error) bool {
	var r *protocol.Refusal
	return errors.As(err, &r) && r.Code == protocol.CodeRequestConflict
}

// isEndpointUnreachable reports whether the endpoint is not running (the IPC
// socket is absent or the connection refused). The intent is persisted; the
// drainer replays after restart. ipc.Call returns a typed refusal with
// CodeEndpointUnreachable for a dial failure.
func isEndpointUnreachable(err error) bool {
	var r *protocol.Refusal
	return errors.As(err, &r) && r.Code == protocol.CodeEndpointUnreachable
}

// isNotFound reports whether the endpoint returned not_found for a request
// ref — the record does not exist in the request store (the submit may be
// spooled but not yet dispatched).
func isNotFound(err error) bool {
	var r *protocol.Refusal
	return errors.As(err, &r) && r.Code == protocol.CodeNotFound
}

// isInvalid reports whether the endpoint refused the request as invalid
// (e.g. a bare request ID passed where a ref is required). Round-4 B6 gap 3:
// status falls back to the spool on invalid too, so a bare request ID is
// resolved from the sender spool even while the companion is up.
func isInvalid(err error) bool {
	var r *protocol.Refusal
	return errors.As(err, &r) && r.Code == protocol.CodeInvalid
}

// lookupSpoolStatus checks the sender spool for a request ref OR a raw
// request ID and returns a SpoolReceipt describing the envelope's state.
// B6 (round-4): status accepts the envelope id directly as a positional
// argument (no --request-id flag) because B3 stopped minting refs for
// spooled submits. Returns (zero, false) when no envelope matches.
func lookupSpoolStatus(stateDir, refOrID string) (sender.SpoolReceipt, bool) {
	var creatorHost, requestID string
	if h, _, id, err := protocol.DecodeRef(refOrID); err == nil {
		creatorHost, requestID = h, id
	} else {
		// Not a ref — treat as a raw request ID and scan the spool.
		requestID = refOrID
	}
	spool, err := sender.Open(stateDir)
	if err != nil {
		return sender.SpoolReceipt{}, false
	}
	if creatorHost != "" {
		env, exists, err := spool.Get(creatorHost, requestID)
		if err != nil || !exists {
			return sender.SpoolReceipt{}, false
		}
		return env.Receipt("spool_lookup"), true
	}
	// No creator host: scan all envelopes for the request ID.
	envs, err := spool.List()
	if err != nil {
		return sender.SpoolReceipt{}, false
	}
	for _, env := range envs {
		if env.RequestID == requestID {
			return env.Receipt("spool_lookup"), true
		}
	}
	return sender.SpoolReceipt{}, false
}

// exitForSpoolReceipt maps a spool envelope state to an exit code. A
// pending/dispatched envelope is success (the work is in flight). A
// failed/expired envelope is ExitError: native work did not happen (or its
// window closed) and the caller takes the failure path (round-4 B6: expired
// now exits 1, not 0).
func exitForSpoolReceipt(r sender.SpoolReceipt) int {
	switch r.State {
	case sender.StateFailed, sender.StateExpired:
		return protocol.ExitError
	}
	return protocol.ExitSuccess
}

// exitForState maps a spool-side state to the exit code for a `submitted`
// receipt. A pending/dispatching request is success for submit.
func exitForState(s protocol.State) int {
	switch s {
	case protocol.StateRejected, protocol.StateUncertain:
		return protocol.ExitActionRequired
	case protocol.StateFailed, protocol.StateCancelled:
		return protocol.ExitError
	}
	return protocol.ExitSuccess
}
