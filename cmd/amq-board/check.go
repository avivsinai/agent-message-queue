package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
	return runCheckContext(context.Background(), args)
}

func runCheckContext(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("amq-board check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	asJSON := flags.Bool("json", false, "the floor problems as JSON")
	notify := flags.String("notify", "", "send new floor problems to these comma-separated handles")
	as := flags.String("as", "", "dedicated observer handle for floor notices")
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
	if (*notify == "") != (*as == "") {
		fmt.Fprintln(os.Stderr, "amq-board check: --notify and --as are required together")
		return exitUsage
	}
	var recipients []string
	if *notify == "" {
		state, cfg, err := readPublishedSnapshot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "amq-board check:", err)
			return exitGeneral
		}
		now := time.Now()
		model := state.Project(cfg.Board, now, version)
		problems, err := board.Check(model, cfg.Board, now)
		return writeCheck(*asJSON, now, model, problems, err)
	}
	for handle := range strings.SplitSeq(*notify, ",") {
		handle = strings.TrimSpace(handle)
		if fsq.ValidateHandle(handle) != nil {
			fmt.Fprintf(os.Stderr, "amq-board check: invalid recipient %q\n", handle)
			return exitUsage
		}
		recipients = append(recipients, handle)
	}
	store, env, cfg, session, err := checkNotifyContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board check:", err)
		return exitGeneral
	}
	roster, err := notifyRoster(ctx, env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board check: resolve floor handles:", err)
		return exitGeneral
	}
	report := board.Notify(ctx, board.NotifyPlan{Sender: *as, Recipients: recipients, Roster: roster, Env: env, Session: session, Store: store, Config: cfg.Board})
	code := writeCheck(*asJSON, time.Now(), report.Model, report.Problems, report.Err)
	if report.NoticeErr != nil {
		fmt.Fprintln(os.Stderr, "amq-board check: floor notification:", report.NoticeErr)
		return exitGeneral
	}
	return code
}

func writeCheck(asJSON bool, now time.Time, model board.Model, problems []board.Problem, err error) int {
	if asJSON && err == nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(checkDocument{Schema: "amq/board-check/v1", GeneratedAt: now, PublishedAt: model.PublishedAt, Problems: problems})
	} else if err == nil {
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
