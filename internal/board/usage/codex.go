package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// codexMeta is the rollout's session_meta: the thread id, its working
// directory, and the parent thread of a spawned subagent.
type codexMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID         string          `json:"id"`
		Cwd        string          `json:"cwd"`
		CLIVersion string          `json:"cli_version"`
		Source     json.RawMessage `json:"source"`
	} `json:"payload"`
}

type codexSpawn struct {
	Subagent struct {
		ThreadSpawn struct {
			ParentThreadID string `json:"parent_thread_id"`
		} `json:"thread_spawn"`
	} `json:"subagent"`
}

type codexUsage struct {
	In         int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Out        int64 `json:"output_tokens"`
	Total      int64 `json:"total_tokens"`
}

// codexTokenCount is one token_count event. total_token_usage is the
// thread's cumulative count; consecutive events can repeat it or report an
// older, lower total, so spend is the rise above the highest total so far,
// never a sum of last_token_usage.
type codexTokenCount struct {
	Timestamp time.Time `json:"timestamp"`
	Payload   struct {
		Type string `json:"type"`
		Info *struct {
			Total codexUsage `json:"total_token_usage"`
		} `json:"info"`
		RateLimits *struct {
			Primary   json.RawMessage `json:"primary"`
			Secondary json.RawMessage `json:"secondary"`
			Credits   *struct {
				Balance *string `json:"balance"`
			} `json:"credits"`
			ReachedType *string `json:"rate_limit_reached_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

var codexKeys = [][]byte{[]byte(`"token_count"`)}

// metaLines bounds how far into a rollout the reader looks for its
// session_meta. A rollout without one has no working directory and is never
// eligible.
const metaLines = 16

// backStep is how far the window offset moves back when the first total it
// read is already inside the window, so the delta has its baseline.
const backStep = 4 << 20

// codexAccount collects the account facts across rollouts: the newest event
// wins.
type codexAccount struct {
	balance     string
	balanceAt   time.Time
	newestAt    time.Time
	reached     string
	windowsSeen bool
	// reportSeen is set by any rate limit report, with or without windows.
	reportSeen bool
	// unknown is set when an eligible rollout has an unverified version;
	// its events, limits included, were not read.
	unknown string
}

func readCodex(acct *Account, files []file, now time.Time, eligible Eligible, b *budget) {
	since := now.Add(-span)
	threads := map[string]*Session{}
	parents := map[string]string{}
	var facts codexAccount
	for _, fl := range files {
		if b.spent() {
			break
		}
		f, err := os.Open(fl.path)
		if err != nil {
			continue // unread: the totals say "at least"
		}
		switch readRollout(f, since, eligible, b, &acct.Skipped, threads, parents, &facts) {
		case readDone:
			acct.Read++
		case readUnverified:
			acct.Unverified++
		}
		_ = f.Close()
	}
	// A child thread's spend rolls up to its root through recorded parents;
	// a parent outside the read set still collects it under its own id.
	roots := map[string]*Session{}
	for id, s := range threads {
		root := id
		for hops := 0; hops < 64; hops++ {
			p, ok := parents[root]
			if !ok || p == "" {
				break
			}
			root = p
		}
		r := roots[root]
		if r == nil {
			r = &Session{Harness: Codex, ID: root}
			roots[root] = r
		}
		r.events = append(r.events, s.events...)
		if r.Unknown == "" {
			r.Unknown = s.Unknown
		}
	}
	acct.Sessions = sortedSessions(roots)
	if facts.reached != "" {
		acct.Limits = append(acct.Limits, Limit{Kind: "reached", Name: facts.reached, AsOf: facts.newestAt, Source: "rollout"})
	}
	if facts.balance != "" {
		acct.Limits = append(acct.Limits, Limit{Kind: "credits", Name: "credits", Balance: facts.balance, AsOf: facts.balanceAt, Source: "rollout"})
	}
	var reason string
	switch {
	case facts.unknown != "":
		reason = "limit unknown (" + facts.unknown + ")"
	case facts.windowsSeen:
		reason = "plan windows reported; this version does not read them"
	case facts.reportSeen:
		reason = "windows not reported by this plan"
	default:
		reason = "limit unknown (no rate limit report read)"
	}
	acct.Limits = append(acct.Limits, Limit{Kind: "unknown", Name: "windows", Source: "rollout", Reason: reason})
}

// readOutcome is what reading one session file achieved.
type readOutcome int

const (
	// readIncomplete: not read to the end, or not identified.
	readIncomplete readOutcome = iota
	// readDone: read to the end, or identified as another project's.
	readDone
	// readUnverified: an unverified format version; nothing counted.
	readUnverified
)

// readRollout reads one rollout.
func readRollout(f *os.File, since time.Time, eligible Eligible, b *budget, skipped *int, threads map[string]*Session, parents map[string]string, facts *codexAccount) readOutcome {
	meta, ok := rolloutMeta(f, b)
	if !ok || meta.Payload.ID == "" || meta.Payload.Cwd == "" {
		return readIncomplete
	}
	if !eligible(meta.Payload.Cwd) {
		return readDone
	}
	id := meta.Payload.ID
	var spawn codexSpawn
	if len(meta.Payload.Source) > 0 && json.Unmarshal(meta.Payload.Source, &spawn) == nil {
		if p := spawn.Subagent.ThreadSpawn.ParentThreadID; p != "" && p != id {
			parents[id] = p
		}
	}
	if !codexVersions.has(meta.Payload.CLIVersion) {
		reason := unsupported("Codex", meta.Payload.CLIVersion)
		if threads[id] == nil {
			threads[id] = &Session{Harness: Codex, ID: id}
		}
		threads[id].Unknown = reason
		if facts.unknown == "" {
			facts.unknown = reason
		}
		return readUnverified
	}
	info, err := f.Stat()
	if err != nil {
		return readIncomplete
	}
	off := seekWindow(f, info.Size(), since, b)
	for {
		var evs []event
		var prev *codexUsage
		baselineMissing := false
		first := true
		done := scan(f, off, codexKeys, b, skipped, func(raw []byte) bool {
			var tc codexTokenCount
			if json.Unmarshal(raw, &tc) != nil || tc.Payload.Type != "token_count" {
				return true
			}
			noteRateLimits(tc, facts)
			if tc.Payload.Info == nil {
				return true
			}
			cur := tc.Payload.Info.Total
			if first {
				first = false
				if !tc.Timestamp.Before(since) && off > 0 {
					baselineMissing = true
					return false
				}
			}
			// A total at or below the highest so far is no new spend.
			if prev != nil && cur.Total <= prev.Total {
				return true
			}
			d := cur
			if prev != nil {
				d = codexUsage{In: cur.In - prev.In, Cached: cur.Cached - prev.Cached, CacheWrite: cur.CacheWrite - prev.CacheWrite, Out: cur.Out - prev.Out}
			}
			c := cur
			prev = &c
			if tc.Timestamp.Before(since) {
				return true // the baseline, not counted
			}
			evs = append(evs, event{at: tc.Timestamp, Counts: Counts{In: d.In - d.Cached, CacheRead: d.Cached, CacheWrite: d.CacheWrite, Out: d.Out}})
			return true
		})
		if baselineMissing {
			off = max(0, off-backStep)
			continue
		}
		if len(evs) > 0 {
			s := threads[id]
			if s == nil {
				s = &Session{Harness: Codex, ID: id}
				threads[id] = s
			}
			s.events = append(s.events, evs...)
		}
		if done {
			return readDone
		}
		return readIncomplete
	}
}

// rolloutMeta finds the session_meta among the rollout's first lines.
func rolloutMeta(f *os.File, b *budget) (codexMeta, bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return codexMeta{}, false
	}
	r := bufio.NewReaderSize(f, 1<<16)
	for k := 0; k < metaLines; k++ {
		line, err := readLine(r, b)
		if line != nil {
			var m codexMeta
			if json.Unmarshal(line, &m) == nil && m.Type == "session_meta" {
				return m, true
			}
		}
		if err != nil && !errors.Is(err, errLineTooLong) {
			return codexMeta{}, false
		}
	}
	return codexMeta{}, false
}

// noteRateLimits keeps the account facts of the newest event: the latest
// credit balance seen, whether a limit is reached now, and whether the plan
// reports windows at all.
func noteRateLimits(tc codexTokenCount, facts *codexAccount) {
	rl := tc.Payload.RateLimits
	if rl == nil {
		return
	}
	facts.reportSeen = true
	if rl.Credits != nil && rl.Credits.Balance != nil && tc.Timestamp.After(facts.balanceAt) {
		facts.balance, facts.balanceAt = *rl.Credits.Balance, tc.Timestamp
	}
	if notNull(rl.Primary) || notNull(rl.Secondary) {
		facts.windowsSeen = true
	}
	if tc.Timestamp.After(facts.newestAt) {
		facts.newestAt = tc.Timestamp
		facts.reached = ""
		if rl.ReachedType != nil {
			facts.reached = *rl.ReachedType
		}
	}
}

func notNull(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}
