// Package usage reads the tokens each harness session spent and the limits
// each harness account reports, from the harnesses' own local records, read
// in place. Spent is per harness session, so it can be shown per worker;
// left is per account (one login, one Codex home), so it is never shown on a
// worker. Only session files are opened, by explicit pattern: no harness
// home is walked, so credential files beside the data are never read. Only
// numeric usage fields, ids and timestamps are kept; no transcript text.
package usage

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Harness names.
const (
	Claude = "claude"
	Codex  = "codex"
	Pi     = "pi"
)

// The windows every total is computed for.
const (
	Window5h = "5h"
	Window7d = "7d"
)

// span is how far back a file must have been written to be read: the
// longest window.
const span = 7 * 24 * time.Hour

// Default per-run budget: the reader stops at a line boundary once either is
// spent and marks the totals it could not finish as "at least".
const (
	DefaultBudgetWall  = time.Second
	DefaultBudgetBytes = 300 << 20
)

// maxLine bounds one transcript line; a longer one is skipped and counted.
const maxLine = 16 << 20

// Config says which homes to read and how much a run may spend.
type Config struct {
	// Homes lists, per harness, the home directories to read; each home is
	// its own account.
	Homes       map[string][]string
	BudgetWall  time.Duration
	BudgetBytes int64
}

// DefaultConfig reads the default home of each harness under home.
func DefaultConfig(home string) Config {
	return Config{
		Homes: map[string][]string{
			Claude: {filepath.Join(home, ".claude")},
			Codex:  {filepath.Join(home, ".codex")},
			Pi:     {filepath.Join(home, ".pi")},
		},
		BudgetWall:  DefaultBudgetWall,
		BudgetBytes: DefaultBudgetBytes,
	}
}

// Counts are one event's tokens, normalized: In is uncached input.
type Counts struct {
	In, CacheRead, CacheWrite, Out int64
	// Cost is set only when the harness reported a cost for the event.
	Cost     *float64
	CostKind string
}

// Fresh is the headline figure: uncached input, cache writes and output.
// Cache reads dominate and are shown beside it, never summed into it.
func (c Counts) Fresh() int64 { return c.In + c.CacheWrite + c.Out }

// event is one counted event of a harness session.
type event struct {
	at time.Time
	Counts
}

// Session is what one harness session (with its subagents or child
// threads rolled up into it) spent inside the read span.
type Session struct {
	Harness string
	// ID is the root session id: a Claude sessionId, the root Codex thread,
	// or a pi session id. A mapping from it to a worker is structural.
	ID     string
	events []event
}

// Limit is one account fact about what is left. It is never per worker.
type Limit struct {
	Kind     string     `json:"kind"` // "credits" | "reached" | "unknown"
	Name     string     `json:"name,omitempty"`
	Balance  string     `json:"balance,omitempty"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	AsOf     time.Time  `json:"as_of,omitzero"`
	Source   string     `json:"source"`
	Reason   string     `json:"reason,omitempty"`
}

// Account is one harness home: its limits and the sessions of this project
// it spent tokens in.
type Account struct {
	Key      string
	Harness  string
	Limits   []Limit
	Sessions []Session
	// Files is the number of session files in the read span; Read is how
	// many were read to the end. Fewer means the totals are "at least".
	Files, Read int
	// Skipped counts lines over maxLine, which were not counted.
	Skipped int
	Detail  string
}

// Partial reports whether the account's totals miss files.
func (a Account) Partial() bool { return a.Read < a.Files }

// Report is one run of every reader.
type Report struct {
	Now      time.Time
	Accounts []Account
}

// RosterKey names the worker a harness session runs as.
type RosterKey struct{ Session, Handle string }

// SessionRef names one root harness session: the harness and its session id.
type SessionRef struct{ Harness, ID string }

// Mapping maps a root harness session to its worker. It comes only from
// structural records (a launch conversation, a Codex thread record, an exec
// pid join); a guess from a working directory never attributes spend.
type Mapping map[SessionRef]RosterKey

// Eligible reports whether a harness session's recorded working directory
// belongs to this project. A session that is not eligible is read no further
// than that directory.
type Eligible func(cwd string) bool

// budget is one run's shared allowance.
type budget struct {
	deadline time.Time
	bytes    int64
	clock    func() time.Time
}

func (b *budget) spent() bool { return b.bytes <= 0 || !b.clock().Before(b.deadline) }

// file is one session file in the read span.
type file struct {
	path    string
	mtime   time.Time
	harness string
}

// Read runs every reader over the configured homes. now dates the windows;
// mapped session files are read first, then the newest.
func Read(cfg Config, now time.Time, eligible Eligible, mapping Mapping) Report {
	clock := time.Now
	if cfg.BudgetWall <= 0 {
		cfg.BudgetWall = DefaultBudgetWall
	}
	if cfg.BudgetBytes <= 0 {
		cfg.BudgetBytes = DefaultBudgetBytes
	}
	type home struct {
		harness, path string
		files         []file
	}
	var homes []home
	for _, harness := range []string{Claude, Codex, Pi} {
		for _, h := range cfg.Homes[harness] {
			homes = append(homes, home{harness, h, sessionFiles(harness, h, now.Add(-span))})
		}
	}
	// Each account gets an equal share of what is left, so a large home
	// cannot starve a small one; what an account leaves unspent passes on.
	start := clock()
	bytesLeft := cfg.BudgetBytes
	rep := Report{Now: now}
	for i, h := range homes {
		accounts := int64(len(homes) - i)
		wallLeft := cfg.BudgetWall - clock().Sub(start)
		b := &budget{deadline: clock().Add(wallLeft / time.Duration(accounts)), bytes: bytesLeft / accounts, clock: clock}
		share := b.bytes
		rep.Accounts = append(rep.Accounts, readHome(h.harness, h.path, h.files, now, eligible, mapping, b))
		bytesLeft -= share - max(b.bytes, 0)
	}
	return rep
}

func readHome(harness, home string, files []file, now time.Time, eligible Eligible, mapping Mapping, b *budget) Account {
	acct := Account{Key: harness + ":" + displayHome(home), Harness: harness}
	acct.Files = len(files)
	sort.SliceStable(files, func(i, j int) bool {
		mi, mj := mappedPath(files[i].path, mapping), mappedPath(files[j].path, mapping)
		if mi != mj {
			return mi
		}
		return files[i].mtime.After(files[j].mtime)
	})
	switch harness {
	case Claude:
		readClaude(&acct, files, now, eligible, b)
	case Codex:
		readCodex(&acct, files, now, eligible, b)
	case Pi:
		readPi(&acct, files, now, eligible, b)
	}
	if acct.Partial() {
		acct.Detail = "partial: " + itoa(acct.Read) + " of " + itoa(acct.Files) + " files"
	}
	if acct.Skipped > 0 {
		acct.Detail = joinDetail(acct.Detail, itoa(acct.Skipped)+" lines over 16 MiB not counted")
	}
	return acct
}

// sessionFiles lists the account's session files written inside the span,
// by explicit pattern only.
func sessionFiles(harness, home string, since time.Time) []file {
	var patterns []string
	switch harness {
	case Claude:
		patterns = []string{"projects/*/*.jsonl", "projects/*/*/subagents/agent-*.jsonl"}
	case Codex:
		patterns = []string{"sessions/*/*/*/rollout-*.jsonl"}
	case Pi:
		patterns = []string{"agent/sessions/*/*.jsonl"}
	}
	var out []file
	for _, p := range patterns {
		matches, _ := filepath.Glob(filepath.Join(home, filepath.FromSlash(p)))
		for _, m := range matches {
			info, err := os.Lstat(m)
			if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(since) {
				continue
			}
			out = append(out, file{path: m, mtime: info.ModTime(), harness: harness})
		}
	}
	return out
}

// mappedPath reports whether a file name carries a mapped session id, so
// mapped workers' files are read first. A miss only changes the order.
func mappedPath(path string, mapping Mapping) bool {
	base := filepath.Base(path)
	for ref := range mapping {
		if ref.ID != "" && strings.Contains(base, ref.ID) {
			return true
		}
	}
	return false
}

// displayHome writes a home under the user's home as ~/...
func displayHome(home string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if rel, err := filepath.Rel(h, home); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "~"
			}
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return home
}

// Totals sums events at or after from.
func (s Session) Totals(from time.Time) (Counts, time.Time) {
	var c Counts
	var asOf time.Time
	for _, e := range s.events {
		if e.at.Before(from) {
			continue
		}
		c = add(c, e.Counts)
		if e.at.After(asOf) {
			asOf = e.at
		}
	}
	return c, asOf
}

func add(a, b Counts) Counts {
	a.In += b.In
	a.CacheRead += b.CacheRead
	a.CacheWrite += b.CacheWrite
	a.Out += b.Out
	if b.Cost != nil {
		sum := *b.Cost
		if a.Cost != nil {
			sum += *a.Cost
		}
		a.Cost = &sum
		a.CostKind = mergeKind(a.CostKind, b.CostKind)
	}
	return a
}

// Add sums two totals, merging their cost kinds.
func Add(a, b Counts) Counts { return add(a, b) }

func mergeKind(a, b string) string {
	switch {
	case a == "" || a == b:
		return b
	case b == "":
		return a
	}
	return "mixed estimates and billed"
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func itoa(n int) string { return strconv.Itoa(n) }

// sortedSessions lists sessions by id, so every face is deterministic.
func sortedSessions(m map[string]*Session) []Session {
	out := make([]Session, 0, len(m))
	for _, s := range m {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
