// Command amq-board shows a fleet of coding agents on one board: what waits
// on the owner, what is in flight, what is blocked, and what nobody holds,
// with each agent's liveness and unread mail. It reads AMQ through the amq
// CLI and beads through `bd --readonly --sandbox`; it never writes to any
// source and never runs the commands it prints.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
)

var version = "dev"

const (
	exitGeneral = 1
	exitUsage   = 2
)

const usage = `Usage: amq-board [--json] [--refresh]

Print the fleet board for the AMQ project of the current directory: the
columns Waiting on you, In flight, Blocked, and Unclaimed, the source health
strip, and the roster per session. Sources are read-only: AMQ through the amq
CLI, beads through bd --readonly --sandbox when a .beads directory exists,
GitHub pull requests and issues through one gh api graphql call cached for
the TTL (default 10m) when the origin remote is on github.com, and external
providers registered in ~/.amq/board.json.
Commands on cards are printed for you to copy; amq-board never runs them.

Flags:
  --json      print the board model as JSON (schema amq/board/v1)
  --refresh   ignore the GitHub cache and fetch once
  --version   print the version
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("amq-board", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	asJSON := flags.Bool("json", false, "print the board model as JSON")
	showVersion := flags.Bool("version", false, "print the version")
	refresh := flags.Bool("refresh", false, "ignore the GitHub cache and fetch once")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "amq-board: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return exitUsage
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	env := board.Env{
		Dir:      dir,
		Now:      time.Now,
		Run:      execRunner{},
		LookPath: exec.LookPath,
		IsDir: func(p string) bool {
			info, err := os.Stat(p)
			return err == nil && info.IsDir()
		},
	}
	userPath := ""
	if home, err := amqhome.Dir(); err == nil {
		userPath = filepath.Join(home, board.ConfigFile)
	}
	cfg, warnings := board.LoadConfig(userPath, filepath.Join(dir, ".amq", board.ConfigFile))
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "amq-board: config:", w)
	}

	ctx := context.Background()
	gh := source.GitHub{
		Repos:                    cfg.GitHubRepos,
		Humans:                   cfg.GitHubLogins,
		Identities:               cfg.GitHubIdentities,
		ReadyToMergeWaitsOnHuman: cfg.ReadyToMergeWaitsH,
		TTL:                      cfg.GitHubTTL,
		Refresh:                  *refresh,
	}
	sources := []board.Source{source.AMQ{MaxAge: cfg.ObligationMaxAge}}
	if cfg.SourceBeads {
		beads := source.Beads{Humans: cfg.BeadsAssignees}
		// A bead's external_ref gh-<n> names a PR only when exactly one
		// GitHub repository is in scope.
		if repos := gh.RepoSet(ctx, env); len(repos) == 1 {
			beads.GitHubRepo = repos[0]
		}
		sources = append(sources, beads)
	} else {
		sources = append(sources, board.Disabled{SourceName: "beads"})
	}
	sources = append(sources, source.CodexGoals{})
	if cfg.SourceGitHub {
		if home, err := amqhome.EnsureDir(); err == nil {
			gh.Cache = source.DirCache{Dir: filepath.Join(home, "board", "cache")}
		} else {
			fmt.Fprintln(os.Stderr, "amq-board: GitHub cache off:", err)
		}
		sources = append(sources, gh)
	} else {
		sources = append(sources, board.Disabled{SourceName: "github"})
	}
	for _, p := range cfg.Providers {
		if p.Enabled {
			sources = append(sources, source.External{Provider: p})
		} else {
			sources = append(sources, board.Disabled{SourceName: source.External{Provider: p}.Name()})
		}
	}
	model := board.Run(ctx, env, sources, cfg.Board, version)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(model)
	} else {
		err = board.WriteText(os.Stdout, model)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	return 0
}

// maxToolOutput bounds the stdout of any one source command. Forge responses
// are not truncated on the server, so the bound is generous; providers get
// their own, smaller bound through RunLimited.
const maxToolOutput = 64 << 20

// stderrTail is how much of a command's stderr is kept: the cause is on the
// last line.
const stderrTail = 4 << 10

// execRunner runs argv directly (no shell) with no stdin and returns stdout.
// A failed command's error carries its stderr.
type execRunner struct{}

func (r execRunner) Run(ctx context.Context, dir string, argv []string) ([]byte, error) {
	return r.RunLimited(ctx, dir, argv, maxToolOutput)
}

// pipeCloseDelay bounds how long a finished or cancelled command may keep
// its output pipes open through a descendant that outlived the kill.
const pipeCloseDelay = 200 * time.Millisecond

// RunLimited holds at most maxOut bytes of stdout; past maxOut it kills the
// command instead of buffering more. On cancellation or overflow the whole
// process group is killed and the pipes close within pipeCloseDelay, so a
// descendant that holds stdout cannot carry the call past its deadline.
func (execRunner) RunLimited(ctx context.Context, dir string, argv []string, maxOut int) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	stdout := &limitBuffer{max: maxOut, overflow: cancel}
	var stderr tailBuffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = pipeCloseDelay
	err := cmd.Run()
	if stdout.over {
		return nil, fmt.Errorf("%s: output over %d bytes", argv[0], maxOut)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%s: %w", argv[0], ctxErr)
		}
		// Tools print warnings before the error; the last line is the cause.
		lines := strings.Split(strings.TrimSpace(string(stderr.buf)), "\n")
		if msg := strings.TrimSpace(lines[len(lines)-1]); msg != "" {
			return nil, fmt.Errorf("%s: %s", argv[0], strings.TrimPrefix(msg, argv[0]+": "))
		}
		return nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	return stdout.buf, nil
}

// limitBuffer keeps up to max bytes. The first write past max marks it over
// and calls overflow; later writes are discarded so the writer never blocks.
type limitBuffer struct {
	buf      []byte
	max      int
	over     bool
	overflow func()
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if !l.over && len(l.buf)+len(p) > l.max {
		l.over = true
		l.buf = nil
		l.overflow()
	}
	if !l.over {
		l.buf = append(l.buf, p...)
	}
	return len(p), nil
}

// tailBuffer keeps the last stderrTail bytes written to it and never fails a
// write, so a noisy command neither grows memory nor blocks.
type tailBuffer struct {
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > stderrTail {
		p = p[len(p)-stderrTail:]
	}
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - stderrTail; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return n, nil
}
