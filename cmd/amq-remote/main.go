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

	"github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/remote/amqio"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

var version = "dev"

// stateDirName is the extension directory under the AMQ root that the
// endpoint owns. amq cleanup never touches extension data.
const stateDirName = "extensions/remote"

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
	case protocol.Reply:
		printHuman(w, v.Snapshot)
		if v.Outcome.Code != "" {
			say(w, "outcome=%s\n", v.Outcome.Code)
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
	useFake := fs.Bool("fake", false, "register the deterministic fake runtime as target 'fake'")
	codexSocket := fs.String("codex-socket", "", "unix socket of the running Codex app-server daemon; attaches its loaded threads")
	codexThread := fs.String("codex-thread", "", "attach only this Codex thread id (with --codex-socket)")
	codexApprove := fs.Bool("codex-approve", false, "advertise approve_tool for Codex threads (only after approval fanout is verified live)")
	poll := fs.Duration("poll", 500*time.Millisecond, "AMQ import and reconciliation interval")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return protocol.ExitUsage, err
	}
	store, err := requests.Open(stateDir)
	if err != nil {
		return 0, err
	}
	var carrier *amqio.Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
		if carrier == nil {
			return nil
		}
		return carrier.Publish(s, origin)
	}})
	carrier, err = amqio.New(c.root, *me, ep)
	if err != nil {
		_ = store.Close()
		return 0, err
	}
	// A cross-project caller must be answered in ITS root, not ours. The
	// carrier holds only the contract; .amqrc discovery, the peer map and
	// session layout stay in the package that owns them.
	root := c.root
	carrier.SetReplyRouter(func(replyProject, replyTo string) (string, string, error) {
		return cli.ResolveReplyRoute(root, replyProject, replyTo)
	})
	if *useFake {
		ep.Register(fake.New("fake", "e_1"))
	}
	if *codexSocket != "" {
		codex.Version = version
		threads := []string{*codexThread}
		if *codexThread == "" {
			threads, err = codex.LoadedThreads(*codexSocket)
			if err != nil {
				_ = ep.Close()
				return 0, fmt.Errorf("list codex threads: %w", err)
			}
		}
		for _, id := range threads {
			att, err := codex.Attach(*codexSocket, id, codex.WithApprovals(*codexApprove))
			if err != nil {
				say(stderr, "codex thread %s: %v\n", id, err)
				continue
			}
			ep.Register(att)
		}
	}
	if err := ep.Reconcile(); err != nil {
		_ = ep.Close()
		return 0, fmt.Errorf("reconcile: %w", err)
	}
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
	session, err := inspectTarget(stateDir, target)
	if err != nil {
		return nil, 0, err
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
		Epoch:     session.Epoch,
		NotAfter:  protocol.FormatTime(time.Now().Add(*window)),
		Input:     &protocol.SubmitInput{Text: body, Busy: protocol.Busy(*busy), Deliver: protocol.Deliver(*deliver)},
	}
	rep, err := callReply(stateDir, cmd)
	if err != nil {
		return nil, 0, err
	}
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
	rep, err := callReply(stateDir, &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: pos[0]})
	if err != nil {
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
	rep, err := callReply(stateDir, &protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestCancel,
		RequestRef: ref,
		TargetID:   targetID,
		Epoch:      session.Epoch,
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
