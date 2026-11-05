package main

import (
	"flag"
	"io"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

const mirrorUsage = "usage: amq-remote mirror on|off|status --self|--name NAME"

// mirrorCommand is `amq-remote mirror on|off|status`: the same switch as
// typing `mirror on` or `mirror off` in the session's Buzz channel. The
// machine agent mirrors only Claude sessions it serves a channel for.
func mirrorCommand(args []string, stdout io.Writer, probe ...*jsonProbe) (int, error) {
	fs := flag.NewFlagSet("mirror", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	self := fs.Bool("self", false, "the session this runs in")
	name := fs.String("name", "", "the session with this binding name")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	*name = strings.TrimSpace(*name)
	if len(rest) != 1 || *self == (*name != "") {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, mirrorUsage)
	}
	verb := rest[0]
	if verb != "on" && verb != "off" && verb != "status" {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, mirrorUsage)
	}
	match := func(b binding.Binding) bool { return b.Name == *name }
	if *self {
		var code int
		if match, code, err = selfMatch(c); err != nil {
			return code, err
		}
	}
	all, err := binding.List()
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	var b binding.Binding
	found := false
	for _, x := range all {
		if match(x) {
			b, found = x, true
			break
		}
	}
	if !found {
		return protocol.ExitNotFound, protocol.Refuse(protocol.CodeNotFound, "not connected: run /amq-remote (amq-remote attach --self) first")
	}
	if b.Harness != "claude" {
		return protocol.ExitActionRequired, protocol.Refuse(protocol.CodeUnsupported, "the mirror shows Claude sessions only; %s is a %s session", b.Name, b.Harness)
	}
	if verb != "status" {
		if err := mirror.SetOff(b.Name, verb == "off"); err != nil {
			return protocol.ExitActionRequired, err
		}
	}
	dir, err := mirror.DirFor(b.Name)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	state := "on"
	if mirror.Off(dir) {
		state = "off"
	}
	_, served := acp.MachineChannelOf(b)
	if c.json {
		emitJSON(stdout, map[string]any{"name": b.Name, "mirror": state, "channel": served})
		return 0, nil
	}
	label := b.Label
	if label == "" {
		label = b.Name
	}
	say(stdout, "Mirror %s for %s.", state, label)
	if !served {
		say(stdout, "The machine agent does not serve a channel for this session, so nothing is posted until it does.")
	}
	return 0, nil
}
