// Command amq-remote is the AMQ Remote companion: an endpoint that attaches to
// running harness sessions and a CLI that submits, follows, and cancels
// requests against it. It is a companion beside amq, like amq-bridge and
// amq-keepalive; amq itself gains no socket. See docs/adr-remote-control.md.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/sender"
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
  up                       Supervise serve: respawn on crash with backoff
  sessions                 List shared runtimes and their capabilities
  inspect TARGET           Current state of one runtime
  submit TARGET            Submit one work request; prints its receipt
  status REQUEST_REF       Current recorded snapshot
  wait REQUEST_REF         Wait for that request's outcome
  cancel REQUEST_REF       Cancel that exact request
  requests                 Recent local request records
  share --session ID       Mint the session body key and print its NIP-OA
                           preimage for the owner to sign (--renew reprints,
                           --tag-file enrolls the signed tag)
  attach --self            Bind your AMQ Remote Buzz agent to the session this
                           runs in (starts the endpoint when none runs)
  detach --self|--name N|--all
                           Unbind; the session keeps running
  bindings                 Bound sessions: live, unknown, or ended (pruned)
  channel reset NAME       Give up session NAME's Buzz channel; the machine
                           agent makes a new one
  service install|uninstall|status [--root R]
                           Start amq-remote up for R (default the machine
                           root) at login: launchd on macOS, systemd on Linux
  link add NAME URL --code CODE
                           Link this machine to a server; type the passkey
                           fingerprint your browser shows
  attach --self --link N   Share the session this runs in with linked server N
                           (--consent passkey|local, --tools PROFILE)
  link keys add NAME --code CODE
                           Accept one more consent passkey (type its fingerprint)
  link local-key NAME      Create the local passkey that confirms consent-local tasks
  link confirm ID          Show a held task's exact text and confirm it (Touch ID)
  link remove NAME         Retire link NAME's sink and delete its device key
  link status              Links on this machine, their sockets and owed
                           revisions, retired sinks
  link tools               The tools a linked server offers this machine
  link call TOOL --args J  Call a linked server's tool (--wait for the final state,
                           --idempotency-key K; resume K after a lost answer)
  mcp --link NAME          Serve a linked server's tools as a stdio MCP server
  doctor                   Diagnose the endpoint chain
  version                  Print the version

Common options:
  --root DIR    AMQ root (default: AM_ROOT; a link command finds its link in
                ~/.amq/remote/root or AM_ROOT)
  --json        Machine-readable output on stdout; diagnostics on stderr

Exit codes: 0 ok · 1 work failed or cancelled · 2 usage · 3 not found ·
4 timeout · 6 action required (busy, unsupported, unshared, expired,
uncertain) · 130 interrupted
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type common struct {
	root string
	json bool
	// rootSet: --root was given on the command line, so a link command uses
	// that root and searches no other.
	rootSet bool
}

// addCommon registers --root and --json. A handler that run() calls passes
// the run's probe, which records the parsed --json value so the result and
// the error are printed in one mode, and whether the parse stopped at -h.
func addCommon(fs *flag.FlagSet, probe ...*jsonProbe) *common {
	c := &common{root: os.Getenv("AM_ROOT")}
	fs.Var(&rootValue{c: c}, "root", "AMQ root directory (default AM_ROOT)")
	jv := &jsonValue{c: c}
	if len(probe) > 0 && probe[0] != nil {
		p := probe[0]
		jv.p = p
		p.fs, p.c = fs, c
		// The flag package calls Usage for -h and after every other parse
		// error; only the other errors write a message to the output first.
		fs.SetOutput(&p.parseOut)
		fs.Usage = func() { p.help = p.parseOut.Len() == 0 }
	}
	fs.Var(jv, "json", "emit JSON")
	return c
}

// rootValue is the --root flag; it records that the flag was given.
type rootValue struct{ c *common }

func (v *rootValue) String() string {
	if v == nil || v.c == nil {
		return ""
	}
	return v.c.root
}

func (v *rootValue) Set(s string) error {
	v.c.root, v.c.rootSet = s, true
	return nil
}

// jsonValue is the --json boolean flag; it reports to the run's probe when
// the flag package sets it.
type jsonValue struct {
	c *common
	p *jsonProbe
}

func (v *jsonValue) IsBoolFlag() bool { return true }

func (v *jsonValue) String() string {
	if v == nil || v.c == nil {
		return "false"
	}
	return strconv.FormatBool(v.c.json)
}

func (v *jsonValue) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	v.c.json = b
	if v.p != nil {
		v.p.seen = true
	}
	return nil
}

// jsonProbe carries one command's --json decision from its own flag parse to
// the output step, so a result and an error never disagree. It also carries
// whether the parse stopped at -h or --help, so run prints the flags.
type jsonProbe struct {
	fs       *flag.FlagSet
	c        *common
	seen     bool
	help     bool
	parseOut bytes.Buffer
}

// wantJSON is the parsed --json value. When the parse failed before it
// reached --json, it walks the remaining arguments by the command's own flag
// definitions, so the value of a string option is never read as a flag.
func (p *jsonProbe) wantJSON(args []string) bool {
	if p == nil || p.fs == nil {
		return false
	}
	if p.seen {
		return p.c.json
	}
	on := false
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}
		if len(tok) < 2 || tok[0] != '-' {
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(tok, "-"), "-"), "=")
		f := p.fs.Lookup(name)
		if f == nil {
			continue
		}
		if name == "json" {
			on = true
			if hasVal {
				b, err := strconv.ParseBool(val)
				on = err == nil && b
			}
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); !hasVal && (!ok || !bf.IsBoolFlag()) {
			i++
		}
	}
	return on
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
		say(stdout, "amq-remote %s", version)
		return 0
	}
	cmd, rest := args[0], args[1:]
	var err error
	var out any
	var code int
	probe := &jsonProbe{}
	switch cmd {
	case "serve":
		code, err = serve(rest, stdout, stderr)
		return finish(stdout, stderr, nil, false, code, err)
	case "up":
		code, err = up(rest, stdout, stderr, newExecSpawner)
		return finish(stdout, stderr, nil, false, code, err)
	case "sessions":
		out, code, err = clientSimple(rest, protocol.OpSessionList, "", probe)
	case "inspect":
		out, code, err = clientSimple(rest, protocol.OpSessionInspect, "TARGET", probe)
	case "submit":
		out, code, err = submit(rest, stdin, probe)
	case "status":
		out, code, err = status(rest, probe)
	case "wait":
		out, code, err = wait(rest, probe)
	case "cancel":
		out, code, err = cancel(rest, probe)
	case "requests":
		out, code, err = listRequests(rest, probe)
	case "share":
		code, err = share(rest, stdout, stderr)
		return finish(stdout, stderr, nil, false, code, err)
	case "attach":
		code, err = attach(rest, stdout, stderr, probe)
	case "detach":
		code, err = detach(rest, stdout, stderr, probe)
	case "bindings":
		code, err = bindings(rest, stdout, probe)
	case "channel":
		code, err = channelCommand(rest, stdout, probe)
	case "service":
		code, err = service(rest, stdout, probe)
	case "doctor":
		out, code, err = doctor(rest, probe)
	case "link":
		out, code, err = link(rest, stdin, stdout, probe)
	case "mcp":
		code, err = mcpServe(rest, stdin, stdout, stderr, probe)
	case "claude":
		// Stop-hook bridge: the receiver subcommand is FAIL-OPEN by
		// contract — exit 0 on ANY error, any parse failure, any panic —
		// because Claude Code treats exit 2 as a blocking error and a
		// broken bridge must never hold the target session hostage.
		return claudeSubcommand(rest, stdin, stdout)
	default:
		say(stderr, "unknown command %q\n%s", cmd, usageText)
		return protocol.ExitUsage
	}
	if probe.help {
		say(stderr, "Usage: amq-remote %s [flags]", probe.fs.Name())
		probe.fs.SetOutput(stderr)
		probe.fs.PrintDefaults()
		return 0
	}
	return finish(stdout, stderr, out, probe.wantJSON(rest), code, err)
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
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// finish prints the reply and maps the outcome to the AMQ exit contract. The
// result and a JSON error go to stdout; a human-readable error goes to stderr.
func finish(stdout, stderr io.Writer, out any, asJSON bool, code int, err error) int {
	if err != nil {
		if asJSON {
			body := map[string]any{"error": err.Error()}
			var r *protocol.Refusal
			if errors.As(err, &r) {
				body["code"] = string(r.Code)
			}
			_ = json.NewEncoder(stdout).Encode(body)
		} else {
			_, _ = fmt.Fprintln(stderr, "amq-remote:", err)
		}
		if code != 0 {
			return code
		}
		return protocol.ExitCode(err)
	}
	if out != nil {
		if asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
		} else {
			printHuman(stdout, out)
		}
	}
	return code
}

func printHuman(w io.Writer, out any) {
	switch v := out.(type) {
	case sender.SpoolReceipt:
		// The sender-side receipt is NOT a Snapshot: it has no request
		// ref or revision. Print it as its own shape.
		say(w, "submitted  %s  (pending, %s)", v.RequestID, v.Reason)
		if v.TargetID != "" {
			say(w, "  target=%s", v.TargetID)
		}
		_, _ = fmt.Fprintln(w)
	case protocol.Reply:
		printHuman(w, v.Snapshot)
		if v.Outcome.Message != "" {
			say(w, "%s", v.Outcome.Message)
		}
		if v.Outcome.Code != "" {
			say(w, "outcome=%s", v.Outcome.Code)
		}
		if v.Outcome.Evidence != "" {
			say(w, "evidence=%s", v.Outcome.Evidence)
		}
		if v.Outcome.Disposition != "" {
			say(w, "cancel=%s", v.Outcome.Disposition)
		}
	case protocol.Snapshot:
		say(w, "%s  %s", v.State, v.RequestRef)
		if v.Code != "" {
			say(w, "  code=%s", v.Code)
		}
		if v.NativeRun != nil {
			say(w, "  run=%s", *v.NativeRun)
		}
		_, _ = fmt.Fprintln(w)
		if v.Result != nil {
			say(w, "%s", v.Result.Text)
			if v.Result.Truncated {
				say(w, "%s", "[truncated; full result stays with the harness]")
			}
		}
	case []protocol.Session:
		for _, s := range v {
			printSession(w, s)
		}
	case protocol.Session:
		printSession(w, v)
	case linkStatusOut:
		printLinkStatus(w, v)
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
	say(w, "%-24s %-8s %-8s %-8s epoch=%s caps=%s observed=%s", s.TargetID, s.Harness, s.Attachment, s.Status, s.Epoch, strings.Join(caps, ","), s.ObservedAt)
}
