package main

import (
	"flag"
	"io"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// channelCommand is `amq-remote channel reset NAME`: the owner gives up the
// machine agent's channel of session NAME, which is the only way to allow a
// new create after one whose outcome Buzz never confirmed. The machine agent
// applies the request on its next scan: it retires the old record, keeps
// looking for a channel that create made (and archives it), and makes a new
// channel. A channel that was lost that way may be left over in Buzz.
func channelCommand(args []string, stdout io.Writer, probe ...*jsonProbe) (int, error) {
	fs := flag.NewFlagSet("channel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if len(rest) != 2 || rest[0] != "reset" {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "usage: amq-remote channel reset NAME")
	}
	name := rest[1]
	ch, err := acp.RequestChannelReset(name)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if c.json {
		emitJSON(stdout, map[string]any{"reset": name, "channel": ch.State, "channel_name": ch.Name})
		return 0, nil
	}
	say(stdout, "Reset requested for %s: the machine agent stops using the %q channel and makes a new one within a minute while it runs. A channel Buzz made but never confirmed may be left over; AMQ archives it if it finds it.", name, ch.Name)
	return 0, nil
}
