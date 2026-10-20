package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/acp/host"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// exitActionRequired is the repository's exit code for a refusal the
// operator must resolve.
const exitActionRequired = 6

// childExitWait bounds how long the host waits for the agent to exit after
// its stdin closes, before it kills the agent's process group.
const childExitWait = 5 * time.Second

// runHost is amq-acp host: it spawns the ACP agent named after `--` and
// serves the AM_ME inbox through it until the agent exits or the host is
// stopped.
func runHost(args []string) int {
	flags := flag.NewFlagSet("amq-acp host", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "working directory for the agent (default: the current directory)")
	flags.Usage = func() { fmt.Fprint(os.Stderr, hostUsage) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	argv := flags.Args()
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, hostUsage)
		return exitUsage
	}

	root := strings.TrimSpace(os.Getenv(acp.EnvRoot))
	if root == "" || !filepath.IsAbs(root) {
		fmt.Fprintf(os.Stderr, "amq-acp host: %s must be an absolute queue root\n", acp.EnvRoot)
		return exitContextMismatch
	}
	root = filepath.Clean(root)
	me := strings.TrimSpace(os.Getenv(acp.EnvMe))
	if err := fsq.ValidateHandle(me); err != nil {
		fmt.Fprintf(os.Stderr, "amq-acp host: %s: %v\n", acp.EnvMe, err)
		return exitContextMismatch
	}
	dir := *cwd
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "amq-acp host:", err)
			return exitGeneral
		}
		dir = wd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host:", err)
		return exitUsage
	}

	// The agent's exit is watched while the host still reads its stdout,
	// so the turn the agent did not survive can name the exit status.
	// StdoutPipe would close the read end on Wait and lose the agent's last
	// lines; an os.Pipe read end stays the host's.
	exitDone := make(chan struct{})
	var exitErr error
	agentExit := func() string {
		select {
		case <-exitDone:
		case <-time.After(childExitWait):
			return "still running"
		}
		if exitErr == nil {
			return "exit status 0"
		}
		return exitErr.Error()
	}

	h, err := host.Open(host.Config{Root: root, Me: me, Cwd: dir, Version: version, Agent: filepath.Base(argv[0]), Log: os.Stderr, AgentExit: agentExit})
	if err != nil {
		return hostExit(err)
	}
	defer h.Close()

	child := exec.Command(argv[0], argv[1:]...)
	child.Dir = dir
	child.Env = childEnv(os.Environ())
	child.Stderr = os.Stderr
	setProcessGroup(child)
	stdin, err := child.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host:", err)
		return exitGeneral
	}
	stdout, agentStdout, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host:", err)
		return exitGeneral
	}
	defer func() { _ = stdout.Close() }()
	child.Stdout = agentStdout
	err = child.Start()
	_ = agentStdout.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host: start the agent:", err)
		return exitGeneral
	}
	go func() {
		exitErr = child.Wait()
		close(exitDone)
		// A process the agent left behind may hold its stdout open; the
		// host still sees the exit once the agent's last lines had time to
		// arrive.
		time.AfterFunc(childExitWait, func() { _ = stdout.Close() })
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal the host finishes the turn in flight; a
		// second one takes the default action.
		<-ctx.Done()
		stop()
	}()
	runErr := h.Run(ctx, stdout, stdin)

	_ = stdin.Close()
	select {
	case <-exitDone:
	case <-time.After(childExitWait):
	}
	// Kill what is left of the agent's process group on every path: the
	// agent itself if it ignored its closed stdin, or anything it started
	// and left behind after it exited.
	killProcessGroup(child)
	<-exitDone
	if exitErr != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host: the agent exited:", exitErr)
	}
	if runErr != nil {
		return hostExit(runErr)
	}
	return 0
}

func hostExit(err error) int {
	fmt.Fprintln(os.Stderr, "amq-acp host:", err)
	var contextErr *acp.ContextError
	var actionErr *host.ActionRequiredError
	switch {
	case errors.As(err, &contextErr):
		return exitContextMismatch
	case errors.As(err, &actionErr):
		return exitActionRequired
	default:
		return exitGeneral
	}
}

// childEnv is the host's environment without any AM_*, AMQ_* or BUZZ_*
// variable, so the agent can never act as the host's handle or see a Buzz
// identity. The host keeps its own environment.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "AM_") || strings.HasPrefix(name, "AMQ_") || strings.HasPrefix(name, "BUZZ_") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

const hostUsage = `amq-acp host runs an Agent Client Protocol agent as an AMQ participant.
Each message in the AM_ME inbox runs as one ACP turn; the answer is a reply
on the message's thread with refs naming it. Only a question, a review
request, a todo, a brainstorm, or a message with no kind runs a turn.

Usage:
  amq-acp host [--cwd <dir>] -- <agent command> [agent arguments]

Example:
  amq coop exec --no-wake --me grok amq-acp -- host -- grok agent --no-leader stdio

` + host.AutonomyNotice + `

The agent runs without a shell, in --cwd, with every AM_*, AMQ_* and BUZZ_*
variable removed from its environment.
`
