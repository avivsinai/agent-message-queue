package main

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/usagecache"
	"github.com/avivsinai/agent-message-queue/internal/usagetransport/command"
	usagefacts "github.com/avivsinai/agent-message-queue/usage"
	"github.com/avivsinai/agent-message-queue/usage/collect"
	"github.com/avivsinai/agent-message-queue/usage/edge"
)

// Plan limit pacing. A Codex home is read at most once per interval across
// every board run and panel. Each deadline bounds the whole pass:
// `codex --version`, the app-server run, and the app-server's exit after
// its reply; reaping a child killed at the deadline adds at most 0.2 s.
// A one-shot face uses the short deadline, serve the longer one.
const (
	limitsInterval        = 5 * time.Minute
	oneShotLimitsDeadline = 3 * time.Second
	serveLimitsDeadline   = 15 * time.Second
)

// limitsFunc returns the plan limit facts of the given Codex homes.
type limitsFunc func(ctx context.Context, homes []string) usagefacts.Result

// limitsEffects binds the real clock, command runner and ~/.amq/usage.
func limitsEffects() (edge.Effects, error) {
	store, err := usagecache.Open()
	if err != nil {
		return edge.Effects{}, err
	}
	return edge.Effects{Clock: time.Now, Command: command.Run, Store: store}, nil
}

// limitsRequest reads the given Codex homes through the codex on PATH; with
// none on PATH it collects nothing.
func limitsRequest(homes []string, deadline time.Duration) collect.Request {
	req := collect.Request{Interval: limitsInterval, Deadline: deadline, ClientName: "amq-board", ClientVersion: version}
	if bin, err := exec.LookPath("codex"); err == nil {
		req.Codex = collect.Codex{Bin: bin, Homes: homes}
	}
	return req
}

// oneShotLimits reads the fact snapshot, and starts the Codex app-server
// only when a home's reading is due.
func oneShotLimits(ctx context.Context, homes []string) usagefacts.Result {
	fx, err := limitsEffects()
	if err != nil {
		return storeFailure(err)
	}
	r := collect.Read(ctx, fx.Store)
	req := limitsRequest(homes, oneShotLimitsDeadline)
	if !collect.Due(r, req, fx.Clock()) {
		return r
	}
	r, _ = collect.Run(ctx, req, collect.Memory{}, fx)
	return r
}

// serveLimits runs one collection pass per serve refresh and keeps the
// pass's memory for the next.
type serveLimits struct {
	mu  sync.Mutex
	mem collect.Memory
}

func (s *serveLimits) run(ctx context.Context, homes []string) usagefacts.Result {
	fx, err := limitsEffects()
	if err != nil {
		return storeFailure(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, mem := collect.Run(ctx, limitsRequest(homes, serveLimitsDeadline), s.mem, fx)
	s.mem = mem
	return r
}

func storeFailure(err error) usagefacts.Result {
	return usagefacts.Result{Failures: []usagefacts.Fact[usagefacts.Failure]{{
		Value: usagefacts.Failure{Code: usagefacts.FailCacheRead, Detail: err.Error()},
	}}}
}
