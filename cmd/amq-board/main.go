// Command amq-board shows a fleet of coding agents on one board: what waits
// on the owner, what is in flight, what is blocked, and what nobody holds,
// with each agent's liveness and unread mail. It reads AMQ through the amq
// CLI and beads through `bd --readonly --sandbox`; it never writes to any
// source and never runs the commands it prints.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
)

var version = "dev"

const (
	exitGeneral = 1
	exitUsage   = 2
)

const usage = `Usage: amq-board [--json]

Print the fleet board for the AMQ project of the current directory: the
columns Waiting on you, In flight, Blocked, and Unclaimed, the source health
strip, and the roster per session. Sources are read-only: AMQ through the amq
CLI, beads through bd --readonly --sandbox when a .beads directory exists.
Commands on cards are printed for you to copy; amq-board never runs them.

Flags:
  --json      print the board model as JSON (schema amq/board/v1)
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
	sources := []board.Source{source.AMQ{}, source.Beads{}}
	model := board.Run(context.Background(), env, sources, board.DefaultConfig(), version)

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

// execRunner runs argv directly (no shell) with no stdin and returns stdout.
// A failed command's error carries its stderr.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir string, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// Tools print warnings before the error; the last line is the cause.
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		if msg := strings.TrimSpace(lines[len(lines)-1]); msg != "" {
			return nil, fmt.Errorf("%s: %s", argv[0], msg)
		}
		return nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	return stdout.Bytes(), nil
}
