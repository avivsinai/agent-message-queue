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

func runRetro(args []string) int {
	flags := flag.NewFlagSet("amq-board retro", flag.ContinueOnError)
	since := flags.String("since", "14d", "measure requests opened in this duration")
	asJSON := flags.Bool("json", false, "print request history as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "amq-board retro: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return exitUsage
	}
	period, err := board.ParseDuration(*since)
	if err != nil || period <= 0 {
		fmt.Fprintf(os.Stderr, "amq-board retro: --since needs a positive duration, got %q\n", *since)
		return exitUsage
	}
	state, _, err := readPublishedSnapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board retro:", err)
		return exitGeneral
	}
	now := time.Now()
	view := state.Retro(now.Add(-period), now)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(struct {
			Schema      string    `json:"schema"`
			GeneratedAt time.Time `json:"generated_at"`
			PublishedAt time.Time `json:"published_at"`
			board.RetroView
		}{board.RetroSchema, now, state.PublishedAt, view})
	} else {
		err = state.WriteRetroText(os.Stdout, view, *since)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board retro:", err)
		return exitGeneral
	}
	return 0
}
