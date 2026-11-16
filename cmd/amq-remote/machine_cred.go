package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

const machineUsage = `Usage: amq-remote machine request [--valid-for DURATION] [--name NAME] [--agent PUBKEY]
       amq-remote machine enroll --grant FILE`

// The machine grant window is at least 1 hour and at most
// machinecred.MaxValidity; the default is 90 days.
const (
	machineMinValidity     = time.Hour
	machineDefaultValidity = 2160 * time.Hour
)

// machineCommand makes the machine agent's key and enrolls the owner's grant
// for it. The owner key never reaches this command: the owner signs the
// printed request with amq-owner-sign in their own terminal.
func machineCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		say(stderr, "%s", machineUsage)
		return protocol.ExitUsage
	}
	var code int
	var err error
	switch args[0] {
	case "request":
		code, err = machineRequest(args[1:], stdout, stderr)
	case "enroll":
		code, err = machineEnroll(args[1:], stdout)
	default:
		say(stderr, "%s", machineUsage)
		return protocol.ExitUsage
	}
	return finish(stdout, stderr, nil, false, code, err)
}

// machineRequest prints the grant request for the machine agent key. With no
// key it makes one; with one it reuses it, so a second run is a renewal;
// with more than one, --agent names the key.
func machineRequest(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("machine request", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	validFor := fs.Duration("valid-for", machineDefaultValidity, "how long the grant lasts (1h to 8760h, default 2160h)")
	name := fs.String("name", "", "agent name in the request (default \"AMQ Remote @ <host>\")")
	agent := fs.String("agent", "", "agent pubkey to renew when this machine has more than one")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			say(stdout, "%s", machineUsage)
			return 0, nil
		}
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if fs.NArg() != 0 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "unexpected argument %q\n%s", fs.Arg(0), machineUsage)
	}
	if *validFor < machineMinValidity || *validFor > machinecred.MaxValidity {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--valid-for must be 1h to %s, got %s", machinecred.MaxValidity, *validFor)
	}
	if *name == "" {
		*name = defaultMachineName()
	}
	if err := machinecred.CheckName(*name); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--name: %v", err)
	}
	home, err := amqhome.EnsureDir()
	if err != nil {
		return protocol.ExitError, err
	}
	agents, err := machinecred.Agents(home)
	if err != nil {
		return protocol.ExitError, err
	}
	var pub string
	switch {
	case *agent != "":
		if !slices.Contains(agents, *agent) {
			return protocol.ExitNotFound, protocol.Refuse(protocol.CodeNotFound, "no machine agent key %s on this machine", *agent)
		}
		pub = *agent
	case len(agents) == 0:
		if pub, err = machinecred.Mint(home); err != nil {
			return protocol.ExitError, err
		}
	case len(agents) == 1:
		pub = agents[0]
	default:
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "this machine has more than one machine agent key (%s); pass --agent", strings.Join(agents, ", "))
	}
	_, _ = fmt.Fprint(stdout, machinecred.Request(pub, *name, time.Now().Add(*validFor).Unix()))
	say(stderr, "save this output to a file, then in your own terminal run: amq-owner-sign --machine <file> --out <grant.json> --relay <wss-url>")
	return 0, nil
}

// defaultMachineName is "AMQ Remote @ <short host name>".
func defaultMachineName() string {
	if host := binding.Machine(); host != "" {
		return "AMQ Remote @ " + host
	}
	return "AMQ Remote"
}

// machineEnroll stores the owner-signed grant for its agent key.
func machineEnroll(args []string, stdout io.Writer) (int, error) {
	fs := flag.NewFlagSet("machine enroll", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	grant := fs.String("grant", "", "grant file that amq-owner-sign --machine wrote (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			say(stdout, "%s", machineUsage)
			return 0, nil
		}
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if *grant == "" || fs.NArg() != 0 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--grant FILE is required\n%s", machineUsage)
	}
	raw, err := os.ReadFile(*grant)
	if err != nil {
		return protocol.ExitUsage, fmt.Errorf("--grant: %w", err)
	}
	home, err := amqhome.EnsureDir()
	if err != nil {
		return protocol.ExitError, err
	}
	cred, err := machinecred.Enroll(home, raw)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	end, err := cred.Tag.NotAfter()
	if err != nil {
		return protocol.ExitError, err
	}
	say(stdout, "agent pubkey: %s", cred.Key.PublicKeyHex())
	say(stdout, "owner pubkey: %s", cred.Tag.OwnerPubKey)
	say(stdout, "relay: %s", cred.Relay)
	say(stdout, "valid until %s", time.Unix(end, 0).UTC().Format(time.RFC3339))
	return 0, nil
}
