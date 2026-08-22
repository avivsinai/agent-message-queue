// Command amq-acp is a preview Agent Client Protocol (ACP) version 1 companion
// for AMQ. It speaks ACP over stdio, turns each prompt into an ordinary AMQ
// message addressed to AMQ_ACP_TO, and exits when stdin closes. It is a
// separate binary on purpose: amq itself gains no protocol server and no
// listening socket.
//
// This companion speaks ACP version 1 only. It runs no model, executes no
// tools, and reads no files on behalf of a client. Queuing a message is not
// proof that the recipient consumed it; use amq receipts for that.
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

const usage = `amq-acp speaks Agent Client Protocol version 1 over stdio and delivers each
prompt as an AMQ message. It never listens on a socket.

Usage:
  amq-acp            serve ACP v1 on stdin and stdout
  amq-acp --version  print the binary version

Environment:
  AM_ROOT          required absolute queue root
  AM_ME            required sender handle
  AMQ_ACP_TO       required recipient handle for every prompt
  AM_BASE_ROOT     pinned base root, required when a session pin is present
  AM_SESSION       pinned session name
  AM_ROOT_ID       identity token authenticating AM_ROOT
  AM_BASE_ROOT_ID  identity token authenticating AM_BASE_ROOT

BUZZ_* variables are read only to refuse agents!=1 and non-owner inbound, then
stripped before any message is written. A deployment lease is upstream; this
binary does not treat an env string as one.

A session pin that cannot be authenticated is refused with exit code 5.
`

func isVersionArgument(args []string) bool {
	return len(args) == 1 && (args[0] == "-v" || args[0] == "--version" || args[0] == "version")
}
