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
	// One session can hold a mailbox and a native binding; the switch
	// applies to each, as detach --self removes each.
	var matched []binding.Binding
	for _, b := range all {
		if match(b) {
			matched = append(matched, b)
		}
	}
	if len(matched) == 0 {
		return protocol.ExitNotFound, protocol.Refuse(protocol.CodeNotFound, "not connected: run /amq-remote (amq-remote attach --self) first")
	}
	type result struct {
		Name    string `json:"name"`
		Mirror  string `json:"mirror"`
		Channel bool   `json:"channel"`
	}
	var results []result
	claudeOnly := matched[:0:0]
	for _, b := range matched {
		if b.Harness == "claude" {
			claudeOnly = append(claudeOnly, b)
		}
	}
	if len(claudeOnly) == 0 {
		return protocol.ExitActionRequired, protocol.Refuse(protocol.CodeUnsupported, "the mirror shows Claude sessions only; %s is a %s session", matched[0].Name, matched[0].Harness)
	}
	for _, b := range claudeOnly {
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
		results = append(results, result{Name: b.Name, Mirror: state, Channel: served})
		if c.json {
			continue
		}
		label := b.Label
		if label == "" {
			label = b.Name
		}
		say(stdout, "Mirror %s for %s.", state, label)
		if !served {
			say(stdout, "The machine agent does not serve a channel for this session, so nothing is posted until it does.")
		}
	}
	if c.json {
		emitJSON(stdout, map[string]any{"sessions": results})
	}
	return 0, nil
}
