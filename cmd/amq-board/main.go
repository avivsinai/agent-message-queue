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
	boardusage "github.com/avivsinai/agent-message-queue/internal/board/usage"
	usagefacts "github.com/avivsinai/agent-message-queue/usage"
)

var version = "dev"

const oneShotGitHubTimeout = 2 * time.Second

const (
	exitGeneral = 1
	exitUsage   = 2
)

const usage = `Usage: amq-board [--json | --html [--out FILE]]
       amq-board sync [--init] [--watch]
       amq-board suggest --endpoint URL --model qwen3.5:4b --digest HEX [--message KEY]
       amq-board suggestions
       amq-board adopt-suggestion --suggestion ID --choice ID --generation N [--confirm-independent]
       amq-board ignore-suggestion --suggestion ID --generation N
       amq-board serve
       amq-board admit --message KEY --title OUTCOME
       amq-board attach|detach --task ID --message KEY
       amq-board move --from-task ID --task ID --message KEY
       amq-board dismiss --message KEY
       amq-board cancel|reopen --scope task --task ID
       amq-board cancel|reopen --scope action --message KEY
       amq-board adopt --file FILE
       amq-board bind-claude-list|unbind-claude-list --home HOME --list LIST
       amq-board catalogue [--task ID]
       amq-board consolidate --task ID --absorb ID --authority ALIAS|owner --generation N
       amq-board move-alias --alias ALIAS --task ID --donor-authority ALIAS|owner --generation N

Views read the published project snapshot. Only sync reads sources.
Suggestion workers examine one whole message per iteration (up to eight units),
prefer current messages, and skip unchanged attempts. Select an exact message with
suggest --message KEY when you need a specific episode.
First initialization is explicit; an existing or interrupted store needs
recovery and is never replaced. Native Beads and admitted AMQ outcomes
have permanent task IDs. Unresolved messages are outside task counts.
Owner commands make scoped local corrections; they never write sources.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "suggest", "suggestions", "adopt-suggestion", "ignore-suggestion":
			return runSuggestions(args[0], args[1:])
		case "sync":
			return runSync(args[1:])
		case "admit", "attach", "detach", "move", "dismiss", "cancel", "reopen", "adopt", "bind-claude-list", "unbind-claude-list", "consolidate", "move-alias", "catalogue":
			return runOwner(args[0], args[1:])
		}
	}
	if len(args) > 0 && args[0] == "serve" {
		return runServe(args[1:])
	}
	flags := flag.NewFlagSet("amq-board", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	asJSON := flags.Bool("json", false, "print the board model as JSON")
	showVersion := flags.Bool("version", false, "print the version")
	refresh := flags.Bool("refresh", false, "ignore the GitHub cache and fetch once")
	asHTML := flags.Bool("html", false, "print the board as one static HTML page")
	out := flags.String("out", "", "with --html, write the page to this file")
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
	if *asJSON && *asHTML {
		fmt.Fprintln(os.Stderr, "amq-board: --json and --html are exclusive")
		return exitUsage
	}
	if *out != "" && !*asHTML {
		fmt.Fprintln(os.Stderr, "amq-board: --out needs --html")
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
	cfg, warnings := loadConfig(dir)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "amq-board: config:", w)
	}
	if *refresh {
		fmt.Fprintln(os.Stderr, "amq-board: --refresh is now a view reload; use amq-board sync to acquire sources")
	}
	store, err := resolveStore(context.Background(), newEnv(dir, execRunner{}))
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	state, err := store.Read()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	model := state.Project(cfg.Board, time.Now(), version)

	switch {
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(model)
	case *asHTML:
		err = writePage(model, *out)
	default:
		err = board.WriteText(os.Stdout, model)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	return 0
}

// newEnv is what the sources of one refresh may use.
func newEnv(dir string, run board.Runner) board.Env {
	return board.Env{
		Dir:      dir,
		Now:      time.Now,
		Run:      run,
		LookPath: exec.LookPath,
		IsDir: func(p string) bool {
			info, err := os.Stat(p)
			return err == nil && info.IsDir()
		},
	}
}

// loadConfig reads ~/.amq/board.json and the project's .amq/board.json.
func loadConfig(dir string) (board.FileConfig, []string) {
	home, _ := os.UserHomeDir()
	return loadConfigFiles(dir, home)
}

// loadConfigFiles reads the board configuration under home and dir, with
// the user file's "usage" key. A refused usage key is a warning and turns
// usage off, so a mistyped "enabled": false never reads the homes it meant
// to keep closed.
func loadConfigFiles(dir, home string) (board.FileConfig, []string) {
	userPath := ""
	if home != "" {
		userPath = filepath.Join(amqhome.Under(home), board.ConfigFile)
	}
	cfg, warnings := board.LoadConfig(userPath, filepath.Join(dir, ".amq", board.ConfigFile))
	if home == "" {
		return cfg, append(warnings, "no home directory: usage not read")
	}
	st, err := boardusage.LoadSettings(userPath, home)
	if err != nil {
		warnings = append(warnings, "usage not read: "+err.Error())
		st.Enabled = false
	}
	cfg.Usage = st
	return cfg, warnings
}

// sourceOptions are what differ between the one-shot faces and serve.
type sourceOptions struct {
	githubTimeout time.Duration // zero keeps the source parent deadline
	refresh       bool          // ignore the forge cache
	ghAccount     string        // the viewer a token read once belongs to; empty: the configured login
	// provider wraps each enabled external provider; nil reads it on every
	// refresh.
	provider func(source.External) board.Source
	// limits returns the Codex plan limit facts; nil reads none.
	limits      limitsFunc
	claudeLists map[string]board.ClaudeListBinding
}

// boardModel reads every source cfg enables, under its own deadline, and
// builds the board model with the usage of each harness account.
func acquireBoard(ctx context.Context, env board.Env, cfg board.FileConfig, opt sourceOptions) board.Acquisition {
	gh := source.GitHub{
		Repos:                    cfg.GitHubRepos,
		Humans:                   cfg.GitHubLogins,
		Identities:               cfg.GitHubIdentities,
		ReadyToMergeWaitsOnHuman: cfg.ReadyToMergeWaitsH,
		TTL:                      cfg.GitHubTTL,
		Timeout:                  opt.githubTimeout,
		Refresh:                  opt.refresh,
		Account:                  opt.ghAccount,
	}
	sources := []board.Source{source.AMQ{MaxAge: cfg.ObligationMaxAge}}
	if cfg.SourceBeads {
		beads := source.Beads{Humans: cfg.BeadsAssignees, IncludeClosed: true}
		// A bead's external_ref gh-<n> names a PR only when exactly one
		// GitHub repository is in scope.
		if repos := gh.RepoSet(ctx, env); len(repos) == 1 {
			beads.GitHubRepo = repos[0]
		}
		sources = append(sources, beads)
	} else {
		sources = append(sources, board.Disabled{SourceName: "beads"})
	}
	sources = append(sources, source.CodexGoals{}, source.ClaudeTasks{Bindings: opt.claudeLists})
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
		ext := source.External{Provider: p}
		switch {
		case !p.Enabled:
			sources = append(sources, board.Disabled{SourceName: ext.Name()})
		case opt.provider != nil:
			sources = append(sources, opt.provider(ext))
		default:
			sources = append(sources, ext)
		}
	}
	// Plan limits are read beside the sources, so a due app-server run
	// overlaps them instead of adding to the board's time.
	var limits <-chan usagefacts.Result
	if cfg.Usage.Enabled && opt.limits != nil {
		ch := make(chan usagefacts.Result, 1)
		homes := cfg.Usage.Config.Homes[boardusage.Codex]
		go func() { ch <- opt.limits(ctx, homes) }()
		limits = ch
	}
	input := board.Acquire(ctx, env, sources, cfg.Board, version)
	model := withUsage(ctx, board.Build(input), env, cfg.Usage, limits)
	beadsStore := ""
	for _, nr := range input.Results {
		if nr.Name == "beads" {
			beadsStore = nr.Result.Namespace
		}
	}

	return board.Acquisition{Input: input, Usage: model.Usage, BeadsStore: beadsStore}
}

// writePage writes the static page to stdout, or to out through a temporary
// file renamed into place, so a browser never reads a half-written page.
func writePage(m board.Model, out string) error {
	regenerate := []string{"amq-board", "--html"}
	if out == "" {
		return board.WriteHTML(os.Stdout, m, board.Quote(regenerate))
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), "."+filepath.Base(abs)+".*")
	if err != nil {
		return err
	}
	// After the rename the temporary name is gone and Remove fails harmlessly.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := board.WriteHTML(tmp, m, board.Quote(append(regenerate, "--out", abs))); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), abs)
}

// withUsage adds what each harness account spent in this project and the
// limits it reports, as the "usage" key of ~/.amq/board.json configures:
// off, extra homes, and the run's budget. A session counts for the project
// when its recorded working directory is in one of the project's Git
// worktrees (the directory itself outside Git); the worktree list is read
// under ctx within worktreeTimeout. Spend goes to the worker an AMQ conversation record ties
// the harness session to; the rest shows under the account's other
// sessions. A non-nil limits delivers the Codex plan limit facts, which
// replace the limits read from rollouts for the homes they cover.
func withUsage(ctx context.Context, m board.Model, env board.Env, st boardusage.Settings, limits <-chan usagefacts.Result) board.Model {
	if !st.Enabled {
		return m
	}
	roots := []string{env.Dir}
	wctx, cancel := context.WithTimeout(ctx, worktreeTimeout)
	out, err := env.Run.Run(wctx, env.Dir, []string{"git", "worktree", "list", "--porcelain"})
	cancel()
	if err == nil {
		if wts := boardusage.Worktrees(out); len(wts) > 0 {
			roots = wts
		}
	}
	mapping := board.UsageMapping(m.Conversations)
	rep := boardusage.Read(st.Config, env.Now(), roots, mapping)
	if limits != nil {
		rep = board.WithPlanLimits(rep, <-limits, env.Now())
	}
	return board.WithUsage(m, rep, mapping)
}

// worktreeTimeout bounds the one command usage runs, well inside a source's
// deadline.
const worktreeTimeout = 10 * time.Second

// maxToolOutput bounds the stdout of any one source command. Forge responses
// are not truncated on the server, so the bound is generous; providers get
// their own, smaller bound through RunLimited.
const maxToolOutput = 64 << 20

// stderrTail is how much of a command's stderr is kept: the cause is on the
// last line.
const stderrTail = 4 << 10

// execRunner runs argv directly (no shell) with no stdin and returns stdout.
// A failed command's error carries its stderr. A non-empty ghToken is passed
// as GH_TOKEN to gh commands only, never to another tool or a provider.
type execRunner struct {
	ghToken     string
	amqEnv      []string
	amqValidate func(context.Context) error
}

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
func (r execRunner) RunLimited(ctx context.Context, dir string, argv []string, maxOut int) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if argv[0] == "amq" && r.amqEnv != nil {
		cmd.Env = append([]string(nil), r.amqEnv...)
	}
	if r.ghToken != "" && argv[0] == "gh" {
		cmd.Env = append(os.Environ(), "GH_TOKEN="+r.ghToken)
	}
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
