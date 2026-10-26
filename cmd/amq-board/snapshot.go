package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
)

func runSync(args []string) int {
	flags := flag.NewFlagSet("amq-board sync", flag.ContinueOnError)
	init := flags.Bool("init", false, "initialize the project store for the first time")
	watch := flags.Bool("watch", false, "acquire and publish once per minute")
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
	env := newEnv(dir, execRunner{})
	store, env, err := resolveBoardContext(ctx, env)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitGeneral
	}
	if *init {
		if err = store.Init(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitGeneral
		}
	}
	initial, err := store.ReadContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitGeneral
	}
	store.ProjectID = initial.ProjectID
	// Reuse the view's last-good configuration rule, without running a view.
	loader := &board.Live{Load: func() (board.FileConfig, []string) { return loadConfig(dir) }, Now: time.Now, Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	var token ghToken
	memos := map[string]*providerMemo{}
	limits := &serveLimits{}
	for {
		if err = env.Run.(execRunner).amqValidate(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitGeneral
		}
		cfg := loader.ReloadConfig()
		runner := env.Run.(execRunner)
		state, readErr := store.ReadContext(ctx)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, readErr)
			return exitGeneral
		}
		opt := sourceOptions{githubTimeout: oneShotGitHubTimeout, limits: oneShotLimits, claudeLists: state.ClaudeLists}
		if *watch {
			opt.limits = limits.run
		}
		if cfg.SourceGitHub && cfg.TokenOnce {
			runner.ghToken, opt.ghAccount = token.get(ctx, newEnv(dir, runner), loader.Logf)
		}
		opt.provider = func(e source.External) board.Source {
			memo := memos[e.Provider.Name]
			if memo == nil {
				memo = &providerMemo{}
				memos[e.Provider.Name] = memo
			}
			return memoProvider{External: e, memo: memo}
		}
		projectID := state.ProjectID
		acquisition := acquireBoard(ctx, newEnv(dir, runner), cfg, opt)
		acquisition.ProjectID = projectID
		history, messages, evidence := source.ReadHistory(ctx, env, store.BaseRoot)
		acquisition.Input.Results = append(acquisition.Input.Results, board.NamedResult{Name: "amq-history", Result: history})
		acquisition.Messages, acquisition.Evidence = messages, evidence
		if ctx.Err() == nil {
			if err = runner.amqValidate(ctx); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return exitGeneral
			}
			err = store.Commit(ctx, acquisition)
		}
		if ctx.Err() != nil {
			return 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "amq-board sync:", err)
			return exitGeneral
		}
		state, readErr = store.ReadContext(ctx)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, readErr)
			return exitGeneral
		}
		fmt.Fprintf(os.Stderr, "amq-board: published generation %d, %d tasks\n", state.Generation, len(state.Tasks))
		if !*watch {
			return 0
		}
		timer := time.NewTimer(time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		}
	}
}

func runOwner(kind string, args []string) int {
	flags := flag.NewFlagSet("amq-board "+kind, flag.ContinueOnError)
	task := flags.String("task", "", "permanent task ID")
	fromTask := flags.String("from-task", "", "source task for an explicit move")
	message := flags.String("message", "", "exact root#messageID from the published unresolved inbox")
	title := flags.String("title", "", "independently actionable outcome")
	anchor := flags.String("anchor", "", "stable operator admission anchor")
	scope := flags.String("scope", "", "task or action")
	file := flags.String("file", "", "explicit withdrawal adoption JSON")
	home := flags.String("home", "", "canonical native home")
	list := flags.String("list", "", "exact native task list ID")
	bindingKey := flags.String("binding-key", "", "exact stored Claude list binding key for revocation")
	generation := flags.Uint64("generation", 0, "expected published generation for identity correction")
	authority := flags.String("authority", "", "explicit lifecycle authority alias or owner")
	alias := flags.String("alias", "", "exact source alias to move")
	donorAuthority := flags.String("donor-authority", "", "replacement authority for donor")
	destinationAuthority := flags.String("destination-authority", "", "optional explicit replacement authority for destination")
	var absorbed, attachments repeatedStrings
	flags.Var(&absorbed, "absorb", "task ID to consolidate; repeat for more tasks")
	flags.Var(&attachments, "attachment", "exact attachment to move with alias; repeat for more attachments")
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
	case "catalogue":
		state, readErr := store.ReadContext(ctx)
		if readErr != nil {
			err = readErr
			break
		}
		catalogue := state.Catalogue()
		canonical := ""
		if *task != "" {
			canonical, err = state.ResolveTaskID(*task)
			if err != nil {
				break
			}
			var selected []board.TaskCatalogueEntry
			for _, entry := range catalogue {
				if entry.ID == canonical {
					selected = append(selected, entry)
				}
			}
			catalogue = selected
		}
		err = json.NewEncoder(os.Stdout).Encode(struct {
			Generation  uint64                     `json:"generation"`
			Tasks       []board.TaskCatalogueEntry `json:"tasks"`
			RequestedID string                     `json:"requested_id,omitempty"`
			CanonicalID string                     `json:"canonical_id,omitempty"`
			Redirects   map[string]string          `json:"redirects"`
		}{state.Generation, catalogue, *task, canonical, state.Redirects()})
	case "consolidate":
		var id string
		id, err = store.Consolidate(ctx, board.Consolidation{Survivor: *task, Absorb: absorbed, Authority: *authority, ExpectedGeneration: *generation})
		if err == nil {
			fmt.Println(id)
		}
	case "move-alias":
		var id string
		id, err = store.MoveAlias(ctx, board.AliasMove{Alias: *alias, Destination: *task, DonorAuthority: *donorAuthority, DestinationAuthority: *destinationAuthority, Attachments: attachments, ExpectedGeneration: *generation})
		if err == nil {
			fmt.Println(id)
		}
	case "bind-claude-list", "unbind-claude-list":
		var key string
		if kind == "unbind-claude-list" && *bindingKey != "" {
			key = *bindingKey
			err = store.UnbindClaudeListKey(ctx, key)
		} else {
			key, err = store.BindClaudeList(ctx, *home, *list, kind == "unbind-claude-list")
		}
		if err == nil {
			fmt.Println(key)
		}
	case "adopt":
		err = adoptWithdrawals(ctx, store, *file)
	default:
		var id string
		id, err = store.Apply(ctx, board.OwnerAction{Kind: kind, TaskID: *task, FromTaskID: *fromTask, MessageKey: *message, Title: *title, Anchor: *anchor, Scope: *scope})
		if err == nil && id != "" {
			fmt.Println(id)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	return 0
}

func adoptWithdrawals(ctx context.Context, store board.Store, path string) error {
	// The input names exact message anchors, not a claim of owner identity in
	// source text. Running this command is the local operator's adoption.
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var input struct {
		Withdrawals []board.WithdrawalAdoption `json:"withdrawals"`
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		return err
	}
	if len(input.Withdrawals) == 0 {
		return errors.New("adoption contains no exact withdrawal anchors")
	}
	return store.Adopt(ctx, input.Withdrawals)
}

type repeatedStrings []string

func (v *repeatedStrings) String() string         { return strings.Join(*v, ", ") }
func (v *repeatedStrings) Set(value string) error { *v = append(*v, value); return nil }
