package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/decision"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/loginservice"
)

func readPublishedSnapshot() (board.Snapshot, board.FileConfig, error) {
	dir, err := os.Getwd()
	if err != nil {
		return board.Snapshot{}, board.FileConfig{}, err
	}
	cfg, warnings := loadConfig(dir)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "amq-board: config:", w)
	}
	store, err := resolveStore(context.Background(), newEnv(dir, execRunner{}))
	if err != nil {
		return board.Snapshot{}, cfg, err
	}
	state, err := store.Read()
	return state, cfg, err
}

// checkNotifyContext resolves and pins the live project route independently
// of a published snapshot, which is allowed to be stale.
func checkNotifyContext(ctx context.Context) (board.Store, board.Env, board.FileConfig, string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return board.Store{}, board.Env{}, board.FileConfig{}, "", err
	}
	cfg, warnings := loadConfig(dir)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "amq-board: config:", w)
	}
	store, env, err := resolveBoardContext(ctx, newEnv(dir, execRunner{}))
	if err != nil {
		return board.Store{}, board.Env{}, cfg, "", err
	}
	if err := env.Run.(execRunner).amqValidate(ctx); err != nil {
		return board.Store{}, board.Env{}, cfg, "", err
	}
	bc, err := readBoardContext(ctx, env)
	return store, env, cfg, bc.Session, err
}

// notifyRoster asks the live AMQ root directly, under the same command
// binding as delivery. Name is the session name, empty for the base queue.
func notifyRoster(ctx context.Context, env board.Env) ([]board.RosterEntry, error) {
	bc, err := readBoardContext(ctx, env)
	if err != nil {
		return nil, err
	}
	raw, err := env.Run.Run(ctx, env.Dir, []string{"amq", "who", "--json"})
	if err != nil {
		return nil, err
	}
	var scopes []struct {
		Name, Scope, Root string
		Agents            []struct {
			Handle string `json:"handle"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &scopes); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(bc.Root)
	if err != nil {
		return nil, err
	}
	var roster []board.RosterEntry
	for _, scope := range scopes {
		if scope.Root != root && scope.Root != bc.Root {
			continue
		}
		for _, agent := range scope.Agents {
			if fsq.ValidateHandle(agent.Handle) == nil {
				roster = append(roster, board.RosterEntry{Session: bc.Session, Handle: agent.Handle, ValidHandle: true})
			}
		}
	}
	return roster, nil
}

func runSync(args []string) int {
	flags := flag.NewFlagSet("amq-board sync", flag.ContinueOnError)
	init := flags.Bool("init", false, "initialize the project store for the first time")
	suggestions := flags.Bool("suggestions", false, "opt in to provisional suggestions with user-configured provider pins")
	watch := flags.Bool("watch", false, "acquire and publish once per minute")
	session := flags.String("session", "", "resolve the board and floor notices in this AMQ session")
	notify := flags.String("notify", "", "send new floor problems to these comma-separated handles after each pass (with --watch)")
	as := flags.String("as", "", "dedicated observer handle for floor notices (with --watch)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		return exitUsage
	}
	if *notify != "" && !*watch {
		fmt.Fprintln(os.Stderr, "amq-board sync: --notify requires --watch")
		return exitUsage
	}
	recipients, err := watchRecipients(*notify, *as)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board sync:", err)
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
	store, env, err := resolveBoardSession(ctx, env, *session)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitGeneral
	}
	var log io.Writer = os.Stderr
	if *watch {
		log = loginservice.LogSink(filepath.Join(store.Dir, "watch.log"), log)
	}
	if *init {
		if err = store.Init(ctx); err != nil {
			_, _ = fmt.Fprintln(log, err)
			return exitGeneral
		}
	}
	initial, err := store.ReadContext(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(log, err)
		return exitGeneral
	}
	store.ProjectID = initial.ProjectID
	// Reuse the view's last-good configuration rule, without running a view.
	loader := &board.Live{Load: func() (board.FileConfig, []string) { return loadConfig(dir) }, Now: time.Now, Logf: func(f string, a ...any) { _, _ = fmt.Fprintf(log, f+"\n", a...) }}
	token := &ghToken{}
	memos := map[string]*providerMemo{}
	limits := &serveLimits{}
	run := &syncRun{dir: dir, env: env, store: store, loader: loader, token: token, memos: memos, limits: limits, watch: *watch, suggestions: *suggestions}
	floor := floorStep(run, *as, recipients, log)
	var floorPass func(context.Context)
	if floor != nil {
		floorPass = floor.pass
	}
	return watchLoop(ctx, *watch, run.pass, floorPass,
		func(c context.Context) bool {
			timer := time.NewTimer(time.Minute)
			defer timer.Stop()
			select {
			case <-c.Done():
				return false
			case <-timer.C:
				return true
			}
		}, log)
}

// watchRecipients validates the arguments both ways of starting a watch accept.
func watchRecipients(notify, observer string) ([]string, error) {
	if (notify == "") != (observer == "") {
		return nil, errors.New("--notify and --as are required together")
	}
	if notify == "" {
		return nil, nil
	}
	if fsq.ValidateHandle(observer) != nil {
		return nil, fmt.Errorf("invalid observer %q", observer)
	}
	handles := strings.Split(notify, ",")
	for i, h := range handles {
		handles[i] = strings.TrimSpace(h)
		if fsq.ValidateHandle(handles[i]) != nil {
			return nil, fmt.Errorf("invalid recipient %q", handles[i])
		}
	}
	return handles, nil
}

// syncRun holds what one sync process keeps between passes.
type syncRun struct {
	dir         string
	env         board.Env
	store       board.Store
	loader      *board.Live
	token       *ghToken
	memos       map[string]*providerMemo
	limits      *serveLimits
	watch       bool
	suggestions bool
}

// sourceOptions bounds a pass by who waits for it. A one-shot sync has a
// person waiting, so its GitHub read and its limits read are short. A watch
// pass has no one waiting: both keep the source deadline, so a slow answer is
// a result and not a failed read every pass.
func (r *syncRun) sourceOptions() sourceOptions {
	if r.watch {
		return sourceOptions{limits: r.limits.run}
	}
	return sourceOptions{githubTimeout: oneShotGitHubTimeout, limits: oneShotLimits}
}

// pass acquires and publishes one generation and returns the published state.
func (r *syncRun) pass(ctx context.Context) (board.Snapshot, error) {
	env := r.env
	dir := r.dir
	store := r.store
	var err error
	if err = env.Run.(execRunner).amqValidate(ctx); err != nil {
		return board.Snapshot{}, err
	}
	cfg := r.loader.ReloadConfig()
	runner := env.Run.(execRunner)
	state, readErr := store.ReadContext(ctx)
	if readErr != nil {
		return board.Snapshot{}, readErr
	}
	opt := r.sourceOptions()
	opt.claudeLists = state.ClaudeLists
	if cfg.SourceGitHub && cfg.TokenOnce {
		runner.ghToken, opt.ghAccount = r.token.get(ctx, newEnv(dir, runner), r.loader.Logf)
	}
	opt.provider = func(e source.External) board.Source {
		memo := r.memos[e.Provider.Name]
		if memo == nil {
			memo = &providerMemo{}
			r.memos[e.Provider.Name] = memo
		}
		return memoProvider{External: e, memo: memo}
	}
	projectID := state.ProjectID
	acquisition := acquireBoard(ctx, newEnv(dir, runner), cfg, opt)
	acquisition.ProjectID = projectID
	history, messages, evidence := source.ReadHistory(ctx, env, store.BaseRoot, state.Pruned, func(id string) (board.Message, bool) {
		m, ok := state.Messages[store.BaseRoot+"#"+id]
		return m, ok
	})
	acquisition.Input.Results = append(acquisition.Input.Results, board.NamedResult{Name: "amq-history", Result: history})
	acquisition.Messages, acquisition.Evidence = messages, evidence
	if ctx.Err() == nil {
		if err = runner.amqValidate(ctx); err != nil {
			return board.Snapshot{}, err
		}
		err = store.Commit(ctx, acquisition)
	}
	if ctx.Err() != nil {
		return board.Snapshot{}, ctx.Err()
	}
	if err != nil {
		return board.Snapshot{}, fmt.Errorf("amq-board sync: %w", err)
	}
	if r.suggestions {
		cfg = r.loader.ReloadConfig()
		if suggestErr := store.Suggest(ctx, cfg.Suggestions, "", false, func() decision.Config { return r.loader.ReloadConfig().Suggestions }); suggestErr != nil && ctx.Err() == nil {
			r.loader.Logf("amq-board suggestions: %v", suggestErr)
		}
	}
	state, readErr = store.ReadContext(ctx)
	if readErr != nil {
		return board.Snapshot{}, readErr
	}
	return state, nil
}

// watchLoop drives sync passes until the context ends, a failed pass in
// one-shot mode, or board.ErrProjectChanged in watch mode. Every other pass
// failure prints one line and the loop tries again after wait allows it.
// floor runs after every pass when set; its failures never end the loop.
func watchLoop(ctx context.Context, watch bool, pass func(context.Context) (board.Snapshot, error), floor func(context.Context), wait func(context.Context) bool, log io.Writer) int {
	var lastErr string
	for {
		state, err := pass(ctx)
		if ctx.Err() != nil {
			return 0
		}
		switch {
		case err == nil:
			lastErr = ""
			_, _ = fmt.Fprintf(log, "amq-board: published generation %d, %d tasks\n", state.Generation, len(state.Tasks))
		case !watch || errors.Is(err, board.ErrProjectChanged):
			_, _ = fmt.Fprintln(log, err)
			return exitGeneral
		case err.Error() != lastErr:
			// One line when a failure starts or changes; the loop goes on.
			lastErr = err.Error()
			_, _ = fmt.Fprintf(log, "%v; the next pass tries again\n", err)
		}
		if !watch {
			return 0
		}
		if floor != nil {
			floor(ctx)
		}
		if !wait(ctx) {
			return 0
		}
	}
}

func runOwner(kind string, args []string) int {
	flags := flag.NewFlagSet("amq-board "+kind, flag.ContinueOnError)
	task := flags.String("task", "", "permanent task ID")
	fromTask := flags.String("from-task", "", "source task for an explicit move")
	message := flags.String("message", "", "exact root#messageID from the published history")
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
	switch kind {
	case "admit", "attach", "detach", "move", "dismiss":
		if *message == "" {
			fmt.Fprintf(os.Stderr, "amq-board %s: --message KEY is required: the exact root#messageID the card shows\n", kind)
			return exitUsage
		}
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
