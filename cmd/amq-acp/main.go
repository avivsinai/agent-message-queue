// Command amq-acp is an Agent Client Protocol (ACP) version 2 bridge for AMQ.
// It speaks ACP over stdio, turns each prompt into a message on a durable AMQ
// cockpit thread, and holds the turn open for a bounded live reply. It is a
// separate binary on purpose: amq itself gains no protocol server and no
// listening socket.
//
// This companion runs no model, executes no tools, and reads no files on
// behalf of a client. AMQ delivery and reply polling use the pinned context;
// a timeout is returned as a typed no-reply refusal.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/acp"
)

var version = "dev"

// Exit codes follow the repository contract: 1 general, 2 usage, 5 context
// mismatch.
const (
	exitGeneral         = 1
	exitUsage           = 2
	exitContextMismatch = 5
)

func main() {
	if code := run(os.Args[1:]); code != 0 {
		os.Exit(code)
	}
}

func run(args []string) int {
	if isVersionArgument(args) {
		fmt.Println(version)
		return 0
	}
	if len(args) > 0 && args[0] == "install" {
		return runInstall(args[1:])
	}
	if len(args) > 0 && args[0] == "setup" {
		return runSetup(args[1:])
	}
	if len(args) > 0 && args[0] == "host" {
		return runHost(args[1:])
	}

	flags := flag.NewFlagSet("amq-acp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "amq-acp: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return exitUsage
	}

	if err := acp.PrepareWorkerEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp:", err)
		return exitUsage
	}

	cfg, err := acp.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp:", err)
		var contextErr *acp.ContextError
		if errors.As(err, &contextErr) {
			return exitContextMismatch
		}
		return exitGeneral
	}

	if err := acp.NewServer(cfg, version).Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp:", err)
		return exitGeneral
	}
	return 0
}

const usage = `amq-acp speaks Agent Client Protocol version 2 over stdio and bridges
ACP prompts to a durable AMQ cockpit thread, holding each turn open for a live
reply. It never listens on a socket.

Usage:
  amq-acp            serve ACP v2 on stdin and stdout
  amq-acp --version  print the binary version
  amq-acp install --to <handle>
                 write the Buzz Desktop harness for mailbox mode
  amq-acp install --remote-target <target>
                 write the harness with AMQ_ACP_REMOTE_TARGET set
  amq-acp install --remove --to <handle>
                 delete the harness file this command wrote
  amq-acp setup [--out <file>]
                 write the AMQ Remote harness and the Desktop import file
  amq-acp host [--check] [--cwd <dir>] -- <agent command>
                 run an ACP agent as the AM_ME participant: each inbox
                 message is one turn, answered on its thread

Environment:
  AM_ROOT          required absolute queue root
  AM_ME            sender handle, required unless AMQ_ACP_REMOTE_TARGET is set
  AMQ_ACP_TO       recipient handle for every prompt, required unless
                   AMQ_ACP_REMOTE_TARGET is set
  AMQ_ACP_REMOTE_TARGET submit each prompt to this amq-remote target through
                   the endpoint on AM_ROOT, in place of an AMQ message
  AMQ_ACP_REMOTE_NATIVE_SESSION required with AMQ_ACP_REMOTE_TARGET: the
                   shared native session; any other session is refused
  AM_BASE_ROOT     pinned base root, required when a session pin is present
  AM_SESSION       pinned session name
  AM_ROOT_ID       identity token authenticating AM_ROOT
  AM_BASE_ROOT_ID  identity token authenticating AM_BASE_ROOT
  AMQ_ACP_STATE_DIR durable channel/thread state directory under AM_ROOT
  AMQ_ACP_TURN_TIMEOUT bounded reply wait (default 10m)
  AMQ_ACP_POLL_INTERVAL reply poll interval (default 100ms)
  AMQ_ACP_HEARTBEAT_INTERVAL session/update heartbeat interval (default 15s)

BUZZ_* variables are read to refuse non-owner inbound and agents!=1, then
stripped from the environment. agents!=1 is refused in BYOH mode
(AMQ_ACP_TO or AMQ_ACP_REMOTE_TARGET) and on platforms without advisory
locks. With AMQ_ACP_REMOTE=binding any parallelism runs: one worker per Buzz
identity leads the agent's lifecycle. The managed agent identity
(BUZZ_PRIVATE_KEY, BUZZ_AUTH_TAG, BUZZ_RELAY_URL) stays in process memory only to post each
owner-facing answer into its Buzz channel with the buzz CLI
(AMQ_ACP_BUZZ_CLI, else the buzz inside Buzz.app; no implicit PATH fallback). It never
enters an AMQ message.

A session pin that cannot be authenticated is refused with exit code 5.
`

func isVersionArgument(args []string) bool {
	return len(args) == 1 && (args[0] == "-v" || args[0] == "--version" || args[0] == "version")
}
