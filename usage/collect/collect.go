// Package collect is the bounded usage collection procedure. Read returns
// the sanitized fact snapshot and starts nothing. Run is one bounded pass:
// it decides from the journal whether a dispatch is due, takes the one
// collector lock without waiting, reserves the dispatch durably, starts the
// provider's child process, publishes the facts and records completion. No
// work survives the call.
package collect

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/usageformat/codexrpc"
	"github.com/avivsinai/agent-message-queue/internal/usagehold"
	codexnorm "github.com/avivsinai/agent-message-queue/internal/usagenormalize/codex"
	"github.com/avivsinai/agent-message-queue/usage"
	"github.com/avivsinai/agent-message-queue/usage/edge"
)

// Request says what one pass may collect.
type Request struct {
	Codex Codex
	// Interval is the minimum time between two dispatches of the same
	// home.
	Interval time.Duration
	// Deadline bounds one whole pass: the version check, the app-server
	// run and the child's exit. The command runner may take a further
	// moment to reap a killed child.
	Deadline time.Duration
	// ClientName and ClientVersion identify the caller to the app-server.
	ClientName, ClientVersion string
}

// Codex names the Codex CLI and the Codex homes to read limits for. An
// empty Bin collects nothing.
type Codex struct {
	Bin   string
	Homes []string
}

// Memory is the working state a caller hands back to the next pass. Codex
// collection keeps none: every pass starts a new source instance, and all
// pacing lives in the durable journal.
type Memory struct{}

// operation names the Codex dispatch in the journal.
const operation = "codex.app-server.rate-limits"

// versionDeadline bounds `codex --version`, which makes no network call,
// inside the pass deadline.
const versionDeadline = 2 * time.Second

// Output bounds for the two child processes.
const (
	maxVersionOut = 4 << 10
	maxRPCOut     = 1 << 20
)

// Read returns the fact snapshot. An unreadable snapshot is a failure fact,
// not an error.
func Read(_ context.Context, read edge.SnapshotReader) usage.Result {
	r, err := read.LoadFacts()
	if err != nil {
		return infraFailure(usage.FailCacheRead, err)
	}
	return r
}

// Due reports whether facts leave any home of req without a reading inside
// the interval at now. It reads facts only; Run still applies the journal's
// holds.
func Due(facts usage.Result, req Request, now time.Time) bool {
	if req.Codex.Bin == "" {
		return false
	}
	for _, h := range req.Codex.Homes {
		a := usage.Assess(facts, usage.ProviderCodex, h, now, req.Interval)
		if a.Status == usage.StatusUnknown || a.Status == usage.StatusStale {
			return true
		}
	}
	return false
}

// Run is one bounded collection pass. It returns the current facts, the
// newly collected ones included, with any failure of this pass as a failure
// fact; a pass that finds nothing due, or the collector lock held, returns
// the snapshot unchanged.
func Run(ctx context.Context, req Request, mem Memory, fx edge.Effects) (usage.Result, Memory) {
	return run(ctx, req, fx), mem
}

func run(ctx context.Context, req Request, fx edge.Effects) usage.Result {
	facts, err := fx.Store.LoadFacts()
	if err != nil {
		// An unreadable snapshot is replaced by the next publication.
		facts = infraFailure(usage.FailCacheRead, err)
	}
	if req.Codex.Bin == "" || len(req.Codex.Homes) == 0 {
		return facts
	}
	if req.Interval <= 0 || req.Deadline <= 0 {
		return usage.Merge(facts, infraFailure(usage.FailReservation, errors.New("interval and deadline must be positive")))
	}
	// One deadline bounds the whole pass: the version check, the
	// reservation and the app-server run with its exit. The reservation
	// records what is left of it, counted from its own clock reading.
	ctx, cancel := context.WithTimeout(ctx, req.Deadline)
	defer cancel()
	journal, err := fx.Store.LoadJournal()
	if err != nil && !errors.Is(err, edge.ErrCorrupt) {
		return usage.Merge(facts, infraFailure(usage.FailJournal, err))
	}
	// A corrupt journal is set aside below, under the lock; until then it
	// counts as empty.
	if err != nil {
		journal = edge.Journal{}
	}
	if _, ok := usagehold.Next(usage.ProviderCodex, req.Codex.Homes, journal.Attempts, fx.Clock(), usagehold.DefaultRules); !ok {
		return facts
	}
	release, ok, err := fx.Store.TryLock()
	if err != nil {
		return usage.Merge(facts, infraFailure(usage.FailLock, err))
	}
	if !ok {
		return facts
	}
	defer release()

	// Another holder may have collected between the first look and the
	// lock: decide again from what is on disk now.
	var problems usage.Result
	if facts, err = fx.Store.LoadFacts(); err != nil {
		facts, problems = usage.Result{}, infraFailure(usage.FailCacheRead, err)
	}
	if journal, err = fx.Store.LoadJournal(); err != nil {
		if !errors.Is(err, edge.ErrCorrupt) {
			return usage.Merge(facts, usage.Merge(problems, infraFailure(usage.FailJournal, err)))
		}
		kept, aside := fx.Store.SetAsideJournal()
		if aside != nil && kept == "" {
			return usage.Merge(facts, usage.Merge(problems, infraFailure(usage.FailJournal, errors.Join(err, aside))))
		}
		journal = edge.Journal{}
		problems = usage.Merge(problems, infraFailure(usage.FailJournalRecovered, errors.Join(errors.New(err.Error()+"; kept as "+kept), aside)))
	}
	problems = usage.Merge(problems, droppedAttempts(journal))
	now := fx.Clock()
	home, ok := usagehold.Next(usage.ProviderCodex, req.Codex.Homes, journal.Attempts, now, usagehold.DefaultRules)
	if !ok {
		return usage.Merge(facts, problems)
	}
	// The deadline is measured on the monotonic clock and added to the one
	// reading that is also the reservation time, so a wall clock step cannot
	// put the deadline before the reservation.
	notAfter := now
	if d, ok := ctx.Deadline(); ok {
		notAfter = now.Add(max(time.Until(d), 0))
	}
	return usage.Merge(collectCodex(ctx, req, home, facts, journal, now, notAfter, fx), problems)
}

// collectCodex reads one home's limits under the collector lock. ctx and
// notAfter carry the pass deadline.
func collectCodex(ctx context.Context, req Request, home string, facts usage.Result, journal edge.Journal, now, notAfter time.Time, fx edge.Effects) usage.Result {
	attempt := usage.Attempt{
		ID:         usage.ProviderCodex + ":" + home + ":" + strconv.FormatInt(now.UnixNano(), 10),
		Operation:  operation,
		Key:        usage.Account{Provider: usage.ProviderCodex, Home: home},
		ReservedAt: now,
		DeadlineAt: notAfter,
		Interval:   req.Interval,
	}
	ev := usage.Evidence{Source: codexnorm.Source, Contract: codexrpc.Contract, ObservedAt: now}

	// The version check makes no network call, so it runs before the
	// reservation; output that names no version is held like any failure.
	vctx, vcancel := context.WithTimeout(ctx, versionDeadline)
	vr := fx.Command(vctx, edge.Command{Path: req.Codex.Bin, Args: codexrpc.VersionArgs, MaxOut: maxVersionOut})
	vcancel()
	var got usage.Result
	version, versionOK := codexrpc.Version(vr.Stdout)
	switch {
	case vr.Failure != "":
		got = homeFailure(attempt.Key, ev, vr.Failure, "codex --version: "+errText(vr.Err))
	case !versionOK:
		got = homeFailure(attempt.Key, ev, usage.FailUnsupportedVersion, "codex --version printed no version")
	}
	if len(got.Failures) > 0 {
		return finish(facts, got, attempt, fx.Clock(), journal, fx)
	}
	ev.ProducerVersion = usage.Some(version)

	// Codex canonicalizes CODEX_HOME and names the result in its initialize
	// reply, so the run is given the canonical path and checked against it.
	// A home that does not resolve has no login to read.
	codexHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		got = homeFailure(attempt.Key, ev, usage.FailNotLoggedIn, "Codex home: "+errText(err))
		return finish(facts, got, attempt, fx.Clock(), journal, fx)
	}

	journal.Attempts = usagehold.Prune(append(journal.Attempts, attempt), now)
	if err := fx.Store.SaveJournal(journal); err != nil {
		return usage.Merge(facts, homeFailure(attempt.Key, ev, usage.FailReservation, errText(err)))
	}

	// The reservation holds others back only until its deadline: past it,
	// starting the child could overlap a dispatch another process now
	// believes is allowed.
	start := fx.Clock()
	if !start.Before(attempt.DeadlineAt) {
		ev.ObservedAt = start
		got = homeFailure(attempt.Key, ev, usage.FailTimeout, "pass deadline passed before the app-server started")
		return finish(facts, got, attempt, start, journal, fx)
	}
	cctx, cancel := context.WithTimeout(ctx, attempt.DeadlineAt.Sub(start))
	attempt.StartedAt = usage.Some(start)
	reply := fx.Command(cctx, edge.Command{
		Path:   req.Codex.Bin,
		Args:   codexrpc.Args,
		Env:    []string{"CODEX_HOME=" + codexHome},
		Stdin:  codexrpc.Requests(req.ClientName, req.ClientVersion),
		MaxOut: maxRPCOut,
		Done:   codexrpc.Done,
	})
	cancel()
	done := fx.Clock()
	attempt.ExitCode = reply.ExitCode
	ev.ObservedAt = done
	got = codexnorm.Normalize(codexrpc.Parse(reply.Stdout), home, codexHome, ev)
	if reply.Failure != "" {
		got = usage.Merge(homeFailure(attempt.Key, ev, reply.Failure, "codex app-server: "+errText(reply.Err)), got)
	}
	return finish(facts, got, attempt, done, journal, fx)
}

// finish publishes one home's new facts and records the attempt complete.
// New windows or money replace the home's previous facts; a pass that read
// none keeps the previous readings, which then age, beside its failure, and
// a new identity replaces the previous one. A snapshot that was not written
// records the attempt as failed.
func finish(facts, got usage.Result, attempt usage.Attempt, done time.Time, journal edge.Journal, fx edge.Effects) usage.Result {
	home := attempt.Key.Home
	next := got
	if len(got.Windows) == 0 && len(got.Money) == 0 {
		prev := facts.ForHome(usage.ProviderCodex, home)
		prev.Failures = nil
		if len(got.Identities) > 0 {
			prev.Identities = nil
		}
		next = usage.Merge(prev, got)
	}
	facts = usage.Merge(facts.WithoutHome(usage.ProviderCodex, home), next)

	var problems usage.Result
	if err := fx.Store.SaveFacts(facts); err != nil {
		problems = homeFailure(attempt.Key, usage.Evidence{ObservedAt: done}, usage.FailCacheWrite, errText(err))
	}
	attempt.FinishedAt = usage.Some(done)
	switch {
	case len(got.Failures) > 0 && len(got.Windows) == 0 && len(got.Money) == 0:
		// A pass that read limits backs off nothing: its failure stays a
		// fact, and the readings keep their interval.
		attempt.FailureCode = got.Failures[0].Value.Code
	case len(problems.Failures) > 0:
		attempt.FailureCode = usage.FailCacheWrite
	}
	replaced := false
	for i := range journal.Attempts {
		if journal.Attempts[i].ID == attempt.ID {
			journal.Attempts[i], replaced = attempt, true
		}
	}
	if !replaced {
		journal.Attempts = append(journal.Attempts, attempt)
	}
	journal.Attempts = usagehold.Prune(journal.Attempts, done)
	if err := fx.Store.SaveJournal(journal); err != nil {
		problems = usage.Merge(problems, homeFailure(attempt.Key, usage.Evidence{ObservedAt: done}, usage.FailCacheWrite, "journal: "+errText(err)))
	}
	return usage.Merge(facts, problems)
}

// droppedAttempts is the diagnostic for journal attempts the reader rejected;
// the pass goes on without them and its next save removes them.
func droppedAttempts(j edge.Journal) usage.Result {
	var out usage.Result
	for _, d := range j.Dropped {
		out = usage.Merge(out, infraFailure(usage.FailJournalRecovered, errors.New("dropped "+d)))
	}
	return out
}

func homeFailure(acct usage.Account, ev usage.Evidence, code, detail string) usage.Result {
	return usage.Result{Failures: []usage.Fact[usage.Failure]{{Account: acct, Value: usage.Failure{Code: code, Detail: detail}, Evidence: ev}}}
}

// infraFailure is a failure of the collection itself, not of one home: its
// account names no provider.
func infraFailure(code string, err error) usage.Result {
	return usage.Result{Failures: []usage.Fact[usage.Failure]{{Value: usage.Failure{Code: code, Detail: errText(err)}}}}
}

func errText(err error) string {
	if err == nil {
		return "failed"
	}
	return err.Error()
}
