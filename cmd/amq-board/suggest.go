package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/decision"
)

func runSuggestions(kind string, args []string) int {
	flags := flag.NewFlagSet("amq-board "+kind, flag.ContinueOnError)
	endpoint := flags.String("endpoint", "", "explicit literal loopback Ollama endpoint")
	model := flags.String("model", "", "pinned local model name")
	digest := flags.String("digest", "", "full pinned model digest")
	message := flags.String("message", "", "optional exact published message key")
	suggestion := flags.String("suggestion", "", "published suggestion ID")
	generation := flags.Uint64("generation", 0, "expected CURRENT snapshot generation")
	title := flags.String("title", "", "optional owner outcome title")
	confirm := flags.Bool("confirm-independent", false, "confirm this is an independent outcome after reviewing existing and historical candidates")
	var choices repeatedStrings
	flags.Var(&choices, "choice", "selected finite interpretation ID; repeat for every selected ID")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		return exitUsage
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitGeneral
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := resolveStore(ctx, newEnv(dir, execRunner{}))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitGeneral
	}
	switch kind {
	case "suggest":
		cfg := decision.Config{Endpoint: *endpoint, Model: *model, Digest: *digest}
		if *endpoint == "" || *model == "" || *digest == "" {
			fmt.Fprintln(os.Stderr, "suggest requires explicit --endpoint, --model and --digest pins")
			return exitUsage
		}
		err = store.Suggest(ctx, cfg, *message, true, func() decision.Config { return cfg })
	case "adopt-suggestion":
		state, readErr := store.ReadContext(ctx)
		if readErr != nil {
			err = readErr
			break
		}
		store.ProjectID = state.ProjectID
		if state.Suggestions.UserConfigured {
			cfg, warnings := loadConfig(dir)
			if len(warnings) == 0 && cfg.Suggestions != state.Suggestions.Config {
				err = errors.New("suggestion provider configuration changed; run sync --suggestions again")
				break
			}
		}
		_, err = store.AdoptSuggestion(ctx, board.SuggestionAdoption{ID: *suggestion, Choices: choices, ExpectedGeneration: *generation, Title: *title, ConfirmIndependent: *confirm})
	case "ignore-suggestion":
		err = store.IgnoreSuggestion(ctx, *suggestion, *generation)
	case "suggestions":
	default:
		return exitUsage
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board suggestions:", err)
		return exitGeneral
	}
	state, err := store.ReadContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitGeneral
	}
	if err = json.NewEncoder(os.Stdout).Encode(struct {
		Generation  uint64                  `json:"generation"`
		Suggestions []board.Suggestion      `json:"suggestions"`
		Diagnostic  board.SuggestionAttempt `json:"diagnostic"`
	}{state.Generation, state.SuggestionViews(), state.Suggestions.Attempt}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitGeneral
	}
	return 0
}
