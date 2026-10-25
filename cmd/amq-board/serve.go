package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const serveUsage = `Usage: amq-board serve

Serve the board of the AMQ project of the current directory as a live page.
It listens on 127.0.0.1 on a free port and prints the URL once; the URL holds
a random token, and every route without it is 404:

  /<token>/            the page
  /<token>/model.json  the last built model (the same JSON as amq-board --json)
  /<token>/refresh     refresh now

The model is rebuilt every minute; GitHub is fetched at its cache TTL and each
external provider at its TTL. Refresh on the page ignores those caches at most
once a minute; presses within that minute, or while a refresh runs, join it.
~/.amq/board.json and .amq/board.json are read again on every refresh: a
valid edit applies without a restart, and a refused edit keeps the last good
configuration and is reported once. The project directory is fixed at start.
With forges.github.token_once (default true), gh auth token is read once and
held in memory for later gh calls, so a session prompts the keychain once.
While it runs, ~/.amq/board/serve.json (mode 0600) holds its url, token, pid
and start time, so a local client finds it; a file whose pid is not alive is
stale. Stop with Ctrl-C or SIGTERM, which removes the file.
`

func runServe(args []string) int {
	flags := flag.NewFlagSet("amq-board serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, serveUsage) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "amq-board serve: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return exitUsage
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "amq-board: "+format+"\n", a...)
	}

	var token ghToken
	memos := map[string]*providerMemo{}
	live := &board.Live{
		Load: func() (board.FileConfig, []string) { return loadConfig(dir) },
		Now:  time.Now,
		Logf: logf,
		Build: func(ctx context.Context, cfg board.FileConfig, force bool) board.Model {
			runner := execRunner{}
			opt := sourceOptions{refresh: force}
			if cfg.SourceGitHub && cfg.TokenOnce {
				runner.ghToken = token.get(ctx, newEnv(dir, runner), logf)
			}
			opt.provider = func(e source.External) board.Source {
				m := memos[e.Provider.Name]
				if m == nil {
					m = &providerMemo{}
					memos[e.Provider.Name] = m
				}
				return memoProvider{External: e, memo: m, force: force}
			}
			return boardModel(ctx, newEnv(dir, runner), cfg, opt)
		},
	}
	live.Refresh(ctx, false)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	secret, err := randomToken()
	if err != nil {
		_ = ln.Close()
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	host := ln.Addr().String()
	srv := &http.Server{Handler: live.Handler(secret, host), ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf("amq-board: serving http://%s/%s/\n", host, secret)
	record := serveRecord{URL: "http://" + host, Token: secret, PID: os.Getpid(), StartedAt: time.Now().UTC()}
	if err := writeServeFile(record); err != nil {
		logf("serve.json not written; local clients will not find this server: %v", err)
	}
	defer removeServeFile(record.PID)

	go live.Loop(ctx, board.ServeEvery)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
		return 0
	case err := <-served:
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
}

// serveFile is the discovery record of a running serve, below ~/.amq.
const serveFile = "serve.json"

// serveRecord is what a local client needs to read a running serve:
// URL/<token>/model.json. The record is stale when PID is not alive.
type serveRecord struct {
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

func serveDir() (string, error) {
	home, err := amqhome.EnsureDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "board"), nil
}

// writeServeFile replaces ~/.amq/board/serve.json atomically, mode 0600:
// the token in it opens every route of the server.
func writeServeFile(r serveRecord) error {
	dir, err := serveDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(dir, serveFile, append(data, '\n'), 0o600)
	return err
}

// removeServeFile removes serve.json only while it still names pid: a later
// serve that replaced the record keeps it when this one exits.
func removeServeFile(pid int) {
	home, err := amqhome.Dir()
	if err != nil {
		return
	}
	path := filepath.Join(home, "board", serveFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var r serveRecord
	if json.Unmarshal(data, &r) == nil && r.PID == pid {
		_ = os.Remove(path)
	}
}

// randomToken is 128 random bits, hex encoded.
func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ghToken is forges.github.token_once: `gh auth token` is read once per
// process and later gh calls get it as GH_TOKEN, so a serve session reads
// the keychain once. The token stays in memory and reaches gh only. When
// GH_TOKEN or GITHUB_TOKEN is already set, gh uses that and nothing is read.
type ghToken struct {
	once  sync.Once
	token string
}

func (t *ghToken) get(ctx context.Context, env board.Env, logf func(string, ...any)) string {
	t.once.Do(func() {
		if os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" {
			return
		}
		if _, err := env.LookPath("gh"); err != nil {
			return
		}
		out, err := env.Run.Run(ctx, env.Dir, []string{"gh", "auth", "token", "--hostname", "github.com"})
		if err != nil {
			logf("GitHub token not read; each gh call reads it: %v", err)
			return
		}
		t.token = strings.TrimSpace(string(out))
	})
	return t.token
}

// providerMemo is one external provider's last successful output.
type providerMemo struct {
	key string
	at  time.Time
	out []byte
}

// memoProvider runs an external provider at most once per its TTL (minimum
// 60 s, set by the configuration) and rebuilds the result from the kept
// output in between, so no refresh shares items with another. force runs it
// again; an edited exec or args runs it again.
type memoProvider struct {
	source.External
	memo  *providerMemo
	force bool
}

func (p memoProvider) Read(ctx context.Context, env board.Env) board.Result {
	key := p.Provider.Exec + "\x00" + strings.Join(p.Provider.Args, "\x00")
	now := env.Now()
	if m := p.memo; !p.force && m.key == key && now.Sub(m.at) < p.Provider.TTL {
		return p.Build(m.out, nil, m.at)
	}
	out, err := p.Fetch(ctx, env)
	if err == nil {
		*p.memo = providerMemo{key: key, at: now, out: out}
	}
	return p.Build(out, err, now)
}
