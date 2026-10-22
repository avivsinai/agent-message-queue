package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
// stopped. With --check it only probes the agent.
func runHost(args []string) int {
	flags := flag.NewFlagSet("amq-acp host", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "working directory for the agent (default: the current directory)")
	check := flags.Bool("check", false, "run the handshake only, print what the agent reports as JSON, and exit")
	protocol := flags.String("protocol", "acp", "agent protocol: acp (ACP over stdio) or agy-stream (agy's stream-json)")
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
	switch *protocol {
	case "acp":
	case host.ProtocolAgyStream:
		if err := host.AgyArgvRefusal(argv[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "amq-acp host:", err)
			return exitUsage
		}
	default:
		fmt.Fprintf(os.Stderr, "amq-acp host: unknown --protocol %q (want acp or %s)\n", *protocol, host.ProtocolAgyStream)
		return exitUsage
	}
	agy := *protocol == host.ProtocolAgyStream
	if *check && agy {
		return runAgyCheck(argv, dir)
	}
	if *check {
		return runCheck(argv, dir)
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

	// The host lock is taken before the agent starts; AgentExit is read
	// only while Run serves, after the agent has started.
	var agent *agentProcess
	h, err := host.Open(host.Config{Root: root, Me: me, Cwd: dir, Version: version, Agent: filepath.Base(argv[0]), Log: os.Stderr, AgentExit: func() string { return agent.exitStatus() }})
	if err != nil {
		return hostExit(err)
	}
	defer h.Close()
	if agy {
		return runAgyHost(h, argv, dir, &agent)
	}

	agent, err = startAgent(argv, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host:", err)
		return exitGeneral
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal the host finishes the turn in flight; a
		// second one takes the default action.
		<-ctx.Done()
		stop()
	}()
	runErr := h.Run(ctx, agent.stdout, agent.stdin)
	agent.stop()
	if runErr != nil {
		return hostExit(runErr)
	}
	return 0
}

// runCheck probes the agent: initialize only, with no AMQ root, handle, or
// session. It prints the agent's info and capabilities as JSON and exits 0,
// or exits 6 with the reason the host could not serve it.
func runCheck(argv []string, dir string) int {
	agent, err := startAgent(argv, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host:", err)
		return exitActionRequired
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	res, err := host.Check(ctx, agent.stdout, agent.stdin, version)
	stop()
	agent.stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host --check:", err)
		return exitActionRequired
	}
	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host --check:", err)
		return exitGeneral
	}
	fmt.Println(string(out))
	return 0
}

// runAgyHost serves the inbox through agy, which the host starts and
// respawns itself; agent always names the latest agy process.
func runAgyHost(h *host.Host, argv []string, dir string, agent **agentProcess) int {
	spawn := func(extra []string) (host.AgentProcess, error) {
		a, err := startAgent(append(append([]string(nil), argv...), extra...), dir)
		if err != nil {
			return nil, err
		}
		*agent = a
		return a, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := h.RunAgy(ctx, spawn); err != nil {
		return hostExit(err)
	}
	return 0
}

// runAgyCheck starts agy in stream-json mode, prints its init event as JSON,
// and stops it; exit 6 says why agy could not be served.
func runAgyCheck(argv []string, dir string) int {
	spawn := func(extra []string) (host.AgentProcess, error) {
		return startAgent(append(append([]string(nil), argv...), extra...), dir)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	res, err := host.CheckAgy(ctx, spawn)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host --check:", err)
		return exitActionRequired
	}
	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host --check:", err)
		return exitGeneral
	}
	fmt.Println(string(out))
	return 0
}

// agentProcess is the spawned ACP agent. Its stdout is an os.Pipe read end
// the host owns: StdoutPipe would close it on Wait and lose the agent's last
// lines, while the exit is watched so the turn the agent did not survive can
// name the exit status.
type agentProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *os.File
	done   chan struct{}
	err    error
}

// startAgent spawns argv without a shell, in dir, in its own process group,
// with the filtered environment.
func startAgent(argv []string, dir string) (*agentProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = childEnv(os.Environ())
	cmd.Stderr = os.Stderr
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, agentStdout, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = agentStdout
	err = cmd.Start()
	_ = agentStdout.Close()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("start the agent: %w", err)
	}
	a := &agentProcess{cmd: cmd, stdin: stdin, stdout: stdout, done: make(chan struct{})}
	go func() {
		a.err = cmd.Wait()
		close(a.done)
		// A process the agent left behind may hold its stdout open; the
		// host still sees the exit once the agent's last lines had time to
		// arrive.
		time.AfterFunc(childExitWait, func() { _ = stdout.Close() })
	}()
	return a, nil
}

func (a *agentProcess) Stdin() io.WriteCloser { return a.stdin }
func (a *agentProcess) Stdout() io.Reader     { return a.stdout }
func (a *agentProcess) Interrupt() error      { return a.cmd.Process.Signal(os.Interrupt) }
func (a *agentProcess) Stop()                 { a.stop() }

// exitStatus waits briefly for the agent's exit and describes it.
func (a *agentProcess) exitStatus() string {
	select {
	case <-a.done:
	case <-time.After(childExitWait):
		return "still running"
	}
	if a.err == nil {
		return "exit status 0"
	}
	return a.err.Error()
}

// stop closes the agent's stdin, waits up to childExitWait for it to exit,
// then kills what is left of its process group on every path: the agent
// itself if it ignored its closed stdin, or anything it started and left
// behind after it exited.
func (a *agentProcess) stop() {
	_ = a.stdin.Close()
	select {
	case <-a.done:
	case <-time.After(childExitWait):
	}
	killProcessGroup(a.cmd)
	<-a.done
	_ = a.stdout.Close()
	if a.err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp host: the agent exited:", a.err)
	}
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
// variable, so the agent does not inherit the host's queue context or a
// Buzz identity. It runs as the same user, so it can still name the root and
// the host's handle explicitly: this is not an isolation boundary. The host
// keeps its own environment.
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
  amq-acp host --check [--cwd <dir>] -- <agent command> [agent arguments]
  amq-acp host --protocol agy-stream [--check] [--cwd <dir>] -- agy [agy arguments]

Example:
  amq coop exec --no-wake --me grok amq-acp -- host -- grok agent --no-leader stdio

` + host.AutonomyNotice + `

The agent runs without a shell, in --cwd, with every AM_*, AMQ_* and BUZZ_*
variable removed from its environment.

A restarted host resumes the ACP session it recorded when the agent
advertises loadSession; otherwise it opens a new session and says so.

--check runs the handshake only (no AMQ root needed), prints the agent's
info and capabilities as JSON, and exits 0, or 6 with the reason.

--protocol agy-stream drives agy headless over its stream-json mode. The host
starts agy itself with --input-format stream-json --output-format
stream-json -p= (and --conversation to resume), so it refuses those flags
and -c, --continue, --print, --prompt and -i in the agy arguments.
` + host.AgyAutonomyNotice + `
`
