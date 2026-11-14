// Command amq-owner-sign is the owner's one step for a strict relay share or
// for the machine agent. The owner runs it in their own terminal. It reads
// the owner key from the terminal without echo. With --share it signs every
// per-kind NIP-OA grant that one `amq-remote share` output printed and writes
// one bundle file of public signed tags. With --machine it signs the one
// grant that `amq-remote machine request` printed and writes one grant file.
// Both modes then publish the owner's kind 30177 policy for the body.
//
// The key stays inside this process: it is never written, printed, or
// passed to amq or amq-remote. The bundle and the grant hold only public
// values.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"golang.org/x/term"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
)

var version = "dev"

const usage = `Usage: amq-owner-sign --share FILE --out FILE --relay URL [--name NAME]
       amq-owner-sign --machine FILE --out FILE --relay URL [--name NAME]

With --share, signs every grant in one saved ` + "`amq-remote share`" + ` output
with the owner key, writes them as one bundle for
` + "`amq-remote share --bundle`" + `, and publishes the owner's kind 30177 policy
for the body on the relay.

With --machine, signs the one grant in a saved
` + "`amq-remote machine request`" + ` output, writes it for
` + "`amq-remote machine enroll --grant`" + `, and publishes the policy. The grant
lets the agent key act as your Buzz agent for every event kind until the
date it shows.

It first shows the body and every grant it will sign, and asks you to type
the first 8 characters of the body pubkey. Check the body pubkey against the
share output or the request before you confirm. Only then is the owner key
(nsec1... or 64 hex) read from the terminal without echo. It is never
written or printed.

Flags:
  --share FILE    saved output of amq-remote share or share --renew
  --machine FILE  saved output of amq-remote machine request
                  (exactly one of --share and --machine is required)
  --out FILE      bundle or grant file to write, mode 0600 (required)
  --relay URL     relay wss:// URL for the kind 30177 policy (required)
  --name NAME     policy name shown in Buzz (default "AMQ session"; with
                  --machine, the agent name in the request)
  --version       print the version
`

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}

func runMain(args []string, stdout, stderr io.Writer) int {
	// The owner key must never reach a core file, so core dumps are forbidden
	// before anything else runs, and the key is not read if that fails.
	// SetTraceback cannot go below a GOTRACEBACK set in the environment; it
	// only quiets the default. forbidCoreDumps is the guarantee.
	if err := forbidCoreDumps(); err != nil {
		_, _ = fmt.Fprintf(stderr, "amq-owner-sign: cannot forbid core dumps, so the owner key is not read: %v\n", err)
		return 1
	}
	debug.SetTraceback("none")
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v" || args[0] == "version") {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	fs := flag.NewFlagSet("amq-owner-sign", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o options
	fs.StringVar(&o.share, "share", "", "")
	fs.StringVar(&o.machine, "machine", "", "")
	fs.StringVar(&o.out, "out", "", "")
	fs.StringVar(&o.relay, "relay", "", "")
	fs.StringVar(&o.name, "name", "AMQ session", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, usage)
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "amq-owner-sign: %v\n\n%s", err, usage)
		return 2
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "name" {
			o.nameSet = true
		}
	})
	if (o.share == "") == (o.machine == "") || o.out == "" || o.relay == "" || fs.NArg() != 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	if err := relay.ValidateURL(o.relay); err != nil {
		_, _ = fmt.Fprintf(stderr, "amq-owner-sign: %v\n", err)
		return 2
	}
	confirm := confirmOnTerminal("share output")
	if o.machine != "" {
		confirm = confirmOnTerminal("request")
	}
	if err := run(o, confirm, readKeyFromTerminal(stderr), publishPolicy, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "amq-owner-sign: %v\n", err)
		return 1
	}
	return 0
}

type options struct {
	share, out, relay, name string
	// machine is the saved `amq-remote machine request` output; set
	// instead of share.
	machine string
	// nameSet: --name was given, so machine mode does not take the
	// request's agent name for the policy.
	nameSet bool
}

// bundleTag is one signed grant, the shape `amq-remote share --bundle` reads.
type bundleTag struct {
	Kind        uint16 `json:"kind"`
	OwnerPubKey string `json:"owner_pubkey"`
	Conditions  string `json:"conditions"`
	Sig         string `json:"sig"`
}

// grant is one kind's conditions and the preimage `share` printed for it.
type grant struct {
	kind       uint16
	conditions string
	preimage   string
}

// confirmPrefix is how many body pubkey characters the owner types to confirm.
const confirmPrefix = 8

// run shows what the share output or the machine request asks the owner to
// sign and reads the key only after confirm accepts. It signs, writes the
// file, then publishes the policy, so a published policy always names a body
// whose signed file exists.
func run(o options, confirm func(body string) (bool, error), readKey func() ([32]byte, error), publish func(ctx context.Context, url string, evt nostr.Event, sk [32]byte) error, stdout io.Writer) error {
	if o.machine != "" {
		p, err := machinePlan(o, stdout)
		if err != nil {
			return err
		}
		return signWriteAndPublish(p, o, confirm, readKey, publish, stdout)
	}
	raw, err := os.ReadFile(o.share)
	if err != nil {
		return err
	}
	session, body, grants, err := parseShareOutput(string(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", o.share, err)
	}
	if err := printSummary(stdout, session, body, grants); err != nil {
		return err
	}
	return signWriteAndPublish(signPlan{
		body:       body,
		policyName: o.name,
		file:       "bundle",
		sign: func(sk [32]byte) ([]byte, string, error) {
			tags := make([]bundleTag, 0, len(grants))
			for _, g := range grants {
				tag, err := bodykey.SignAuthTag(sk, body, g.conditions)
				if err != nil {
					return nil, "", fmt.Errorf("kind %d: %w", g.kind, err)
				}
				tags = append(tags, bundleTag{Kind: g.kind, OwnerPubKey: tag.OwnerPubKey, Conditions: tag.Conditions, Sig: tag.SigHex()})
			}
			out, err := json.MarshalIndent(tags, "", "  ")
			return out, tags[0].OwnerPubKey, err
		},
		wrote: fmt.Sprintf("wrote %d signed grants to", len(grants)),
		next: func(abs string) string {
			return fmt.Sprintf("amq-remote share --session %s --bundle %s --target <target> --relay %s", session, abs, o.relay)
		},
	}, o, confirm, readKey, publish, stdout)
}

// signPlan is what one mode signs: the body, the policy name, and how to
// sign, name and follow up the file it writes.
type signPlan struct {
	body       string
	policyName string
	// file names the written file in the publish error: "bundle" or "grant".
	file string
	// sign returns the file content and the owner pubkey.
	sign  func(sk [32]byte) ([]byte, string, error)
	wrote string
	next  func(abs string) string
}

// signWriteAndPublish is the sequence both modes share: confirm, read the
// key, sign, write the file, then publish the policy.
func signWriteAndPublish(p signPlan, o options, confirm func(body string) (bool, error), readKey func() ([32]byte, error), publish func(ctx context.Context, url string, evt nostr.Event, sk [32]byte) error, stdout io.Writer) error {
	ok, err := confirm(p.body)
	if err != nil {
		return fmt.Errorf("confirm: %w", err)
	}
	if !ok {
		return errors.New("not confirmed; nothing was signed")
	}
	sk, err := readKey()
	if err != nil {
		return err
	}
	defer func() { sk = [32]byte{} }()

	out, owner, err := p.sign(sk)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "owner pubkey: %s\n", owner)
	evt, err := policyEvent(sk, p.body, p.policyName)
	if err != nil {
		return err
	}

	abs, err := filepath.Abs(o.out)
	if err != nil {
		return err
	}
	// The owner names the output file; its path is theirs, links included.
	if _, err := fsq.WriteFileAtomicFollowLinks(filepath.Dir(abs), filepath.Base(abs), out, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s %s\n", p.wrote, abs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := publish(ctx, o.relay, evt, sk); err != nil {
		return fmt.Errorf("the %s is written to %s, but the kind 30177 policy was not published; run again with the same flags: %w", p.file, abs, err)
	}
	_, _ = fmt.Fprintf(stdout, "published kind 30177 policy %s\nnext: %s\n", evt.ID.Hex(), p.next(abs))
	return nil
}

// machinePlan reads a machine request, refuses a grant end that has passed
// or is more than machinecred.MaxValidity away, and shows the owner what the
// one grant allows.
func machinePlan(o options, stdout io.Writer) (signPlan, error) {
	raw, err := os.ReadFile(o.machine)
	if err != nil {
		return signPlan{}, err
	}
	name, body, conds, err := machinecred.ParseRequest(string(raw))
	if err != nil {
		return signPlan{}, fmt.Errorf("%s: %w", o.machine, err)
	}
	end, err := bodykey.AuthTag{Conditions: conds}.NotAfter()
	if err != nil {
		return signPlan{}, fmt.Errorf("%s: %w", o.machine, err)
	}
	until := time.Unix(end, 0).UTC().Format(time.RFC3339)
	if end <= time.Now().Unix() {
		return signPlan{}, fmt.Errorf("%s: the grant ends at %s, which has passed; run `amq-remote machine request` again", o.machine, until)
	}
	if end > time.Now().Add(machinecred.MaxValidity).Unix() {
		return signPlan{}, fmt.Errorf("%s: the grant ends at %s, more than one year from now; run `amq-remote machine request` with a shorter --valid-for", o.machine, until)
	}
	policyName := o.name
	if !o.nameSet {
		policyName = name
	}
	_, _ = fmt.Fprintf(stdout, "You are about to sign one machine grant and publish a policy for this agent.\nagent:       %s\nbody pubkey: %s\nconditions:  %s\nThis lets the agent key act as your Buzz agent for EVERY event kind until %s.\n", name, body, conds, until)
	return signPlan{
		body:       body,
		policyName: policyName,
		file:       "grant",
		sign: func(sk [32]byte) ([]byte, string, error) {
			tag, err := bodykey.SignAuthTag(sk, body, conds)
			if err != nil {
				return nil, "", err
			}
			out, err := machinecred.EncodeGrant(body, *tag)
			return out, tag.OwnerPubKey, err
		},
		wrote: "wrote the machine grant to",
		next: func(abs string) string {
			return "amq-remote machine enroll --grant " + abs
		},
	}, nil
}

// printSummary shows the owner exactly what they are about to sign: the
// session, the body, and each kind with its signed expiry as a UTC date.
func printSummary(w io.Writer, session, body string, grants []grant) error {
	_, _ = fmt.Fprintf(w, "You are about to sign these grants and publish a policy for this body.\nsession:     %s\nbody pubkey: %s\n", session, body)
	for _, g := range grants {
		conds, err := bodykey.ParseConditions(g.conditions)
		if err != nil {
			return fmt.Errorf("kind %d: %w", g.kind, err)
		}
		until := "no expiry"
		for _, c := range conds {
			if c.CreatedLt != nil {
				until = "until " + time.Unix(int64(*c.CreatedLt), 0).UTC().Format(time.RFC3339)
			}
		}
		_, _ = fmt.Fprintf(w, "  kind %d: %s (%s)\n", g.kind, g.conditions, until)
	}
	return nil
}

// confirmOnTerminal asks the owner, on the controlling terminal, to type the
// first characters of the body pubkey after checking it against source. Any
// other answer, or no terminal, declines.
func confirmOnTerminal(source string) func(body string) (bool, error) {
	return func(body string) (bool, error) {
		return confirmBody(source, body)
	}
}

func confirmBody(source, body string) (bool, error) {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false, fmt.Errorf("a terminal is required to confirm: %w", err)
	}
	defer func() { _ = tty.Close() }()
	_, _ = fmt.Fprintf(os.Stderr, "check the body pubkey against the %s, then type its first %d characters to sign: ", source, confirmPrefix)
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return strings.TrimSpace(line) == body[:confirmPrefix], nil
}

var (
	sessLine = regexp.MustCompile(`^session:\s+(\S+)$`)
	bodyLine = regexp.MustCompile(`^body-pubkey:\s+([0-9a-f]{64})$`)
	preLine  = regexp.MustCompile(`^kind (\d+) preimage: ([0-9a-f]{64})$`)
	condLine = regexp.MustCompile(`^kind (\d+) conditions: (\S+)$`)
)

// parseShareOutput reads the body key and every printed grant. Each printed
// preimage must match the body and conditions, or nothing is signed. A
// dry-run output is refused: its preimages cannot be enrolled.
func parseShareOutput(text string) (session, body string, grants []grant, err error) {
	if strings.Contains(text, "(dry-run") {
		return "", "", nil, errors.New("this is dry-run output; sign the output of a real `amq-remote share` run")
	}
	byKind := map[uint16]*grant{}
	var order []uint16
	at := func(s string) (*grant, error) {
		n, err := strconv.ParseUint(s, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("kind %s: %v", s, err)
		}
		k := uint16(n)
		if byKind[k] == nil {
			byKind[k] = &grant{kind: k}
			order = append(order, k)
		}
		return byKind[k], nil
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if m := sessLine.FindStringSubmatch(line); m != nil {
			session = m[1]
		} else if m := bodyLine.FindStringSubmatch(line); m != nil {
			if body != "" && body != m[1] {
				return "", "", nil, errors.New("more than one body pubkey")
			}
			body = m[1]
		} else if m := preLine.FindStringSubmatch(line); m != nil {
			g, err := at(m[1])
			if err != nil {
				return "", "", nil, err
			}
			g.preimage = m[2]
		} else if m := condLine.FindStringSubmatch(line); m != nil {
			g, err := at(m[1])
			if err != nil {
				return "", "", nil, err
			}
			g.conditions = m[2]
		}
	}
	if body == "" || len(order) == 0 {
		return "", "", nil, errors.New("no body pubkey or grants; save the output of `amq-remote share` and pass it here")
	}
	grants = make([]grant, 0, len(order))
	for _, k := range order {
		g := byKind[k]
		if g.preimage == "" || g.conditions == "" {
			return "", "", nil, fmt.Errorf("kind %d: the output needs both its preimage and conditions lines", k)
		}
		want := hex.EncodeToString(bodykey.AuthTag{Conditions: g.conditions}.Preimage(body))
		if g.preimage != want {
			return "", "", nil, fmt.Errorf("kind %d: the printed preimage does not match the body and conditions", k)
		}
		grants = append(grants, *g)
	}
	return session, body, grants, nil
}

// policyEvent is the owner's kind 30177 policy for the body, the event Buzz
// Desktop reads to list the body as an owned agent.
func policyEvent(sk [32]byte, body, name string) (nostr.Event, error) {
	content, err := json.Marshal(map[string]any{"name": name, "parallelism": 1, "respond_to": "owner-only"})
	if err != nil {
		return nostr.Event{}, err
	}
	evt := nostr.Event{Kind: 30177, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", body}}, Content: string(content)}
	if err := relay.Sign(&evt, sk); err != nil {
		return nostr.Event{}, err
	}
	return evt, nil
}

// publishPolicy authenticates to the relay as the owner and publishes evt.
func publishPolicy(ctx context.Context, url string, evt nostr.Event, sk [32]byte) error {
	r, err := nostr.RelayConnect(ctx, url, nostr.RelayOptions{})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = r.Close() }()
	// Wait for the relay's AUTH challenge; a late challenge fails loudly in r.Auth.
	time.Sleep(time.Second)
	if err := r.Auth(ctx, func(_ context.Context, e *nostr.Event) error { return relay.Sign(e, sk) }); err != nil {
		return fmt.Errorf("auth as owner: %w", err)
	}
	return r.Publish(ctx, evt)
}

// readKeyFromTerminal reads the owner key without echo when stdin is a
// terminal, or one line from stdin otherwise (for a password manager pipe).
func readKeyFromTerminal(prompt io.Writer) func() ([32]byte, error) {
	return func() ([32]byte, error) {
		_, _ = fmt.Fprint(prompt, "owner nsec or hex (hidden): ")
		var line []byte
		var err error
		if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
			line, err = term.ReadPassword(fd)
			_, _ = fmt.Fprintln(prompt)
		} else {
			line, err = bufio.NewReader(os.Stdin).ReadBytes('\n')
			if errors.Is(err, io.EOF) && len(line) > 0 {
				err = nil
			}
		}
		defer clear(line)
		if err != nil {
			return [32]byte{}, fmt.Errorf("read owner key: %w", err)
		}
		return parseKey(strings.TrimSpace(string(line)))
	}
}

func parseKey(s string) ([32]byte, error) {
	if strings.HasPrefix(s, "nsec1") {
		_, v, err := nip19.Decode(s)
		if err != nil {
			return [32]byte{}, errors.New("owner key: not a valid nsec")
		}
		sk, ok := v.(nostr.SecretKey)
		if !ok {
			return [32]byte{}, errors.New("owner key: not a secret key")
		}
		return sk, nil
	}
	sk, err := nostr.SecretKeyFromHex(s)
	if err != nil {
		return [32]byte{}, errors.New("owner key: want nsec1... or 64 hex")
	}
	return sk, nil
}
