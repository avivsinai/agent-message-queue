package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

// exitActionRequired is the exit code for a floor that needs action.
const exitActionRequired = 6

type checkDocument struct {
	Schema      string          `json:"schema"`
	GeneratedAt time.Time       `json:"generated_at"`
	PublishedAt time.Time       `json:"published_at"`
	Problems    []board.Problem `json:"problems"`
}

// runCheck reads the same projected board as the default view, never a source.
func runCheck(args []string) int {
	flags := flag.NewFlagSet("amq-board check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	asJSON := flags.Bool("json", false, "the floor problems as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "amq-board check: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return exitUsage
	}
	state, cfg, err := readPublishedSnapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board check:", err)
		return exitGeneral
	}
	now := time.Now()
	model := state.Project(cfg.Board, now, version)
	problems, err := board.Check(model, cfg.Board, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board check:", err)
		return exitGeneral
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(checkDocument{Schema: "amq/board-check/v1", GeneratedAt: now, PublishedAt: model.PublishedAt, Problems: problems})
	} else {
		for _, p := range problems {
			_, err = fmt.Fprintf(os.Stdout, "%s: %s\n", p.Kind, p.Text)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board check:", err)
		return exitGeneral
	}
	if len(problems) > 0 {
		return exitActionRequired
	}
	return 0
}
