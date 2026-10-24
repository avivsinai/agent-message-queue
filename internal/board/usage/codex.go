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
		ID     string          `json:"id"`
		Cwd    string          `json:"cwd"`
		Source json.RawMessage `json:"source"`
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
// thread's cumulative count; consecutive events can repeat it, so spend is
// the delta between consecutive totals, never a sum of last_token_usage.
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
			acct.Read++
			continue
		}
		done := readRollout(f, since, eligible, b, &acct.Skipped, threads, parents, &facts)
		_ = f.Close()
		if done {
			acct.Read++
		}
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
	}
	acct.Sessions = sortedSessions(roots)
	if facts.reached != "" {
		acct.Limits = append(acct.Limits, Limit{Kind: "reached", Name: facts.reached, AsOf: facts.newestAt, Source: "rollout"})
	}
	if facts.balance != "" {
		acct.Limits = append(acct.Limits, Limit{Kind: "credits", Name: "credits", Balance: facts.balance, AsOf: facts.balanceAt, Source: "rollout"})
	}
	reason := "windows not reported by this plan"
	if facts.windowsSeen {
		reason = "plan windows reported; this version does not read them"
	}
	acct.Limits = append(acct.Limits, Limit{Kind: "unknown", Name: "windows", Source: "rollout", Reason: reason})
}

// readRollout reads one rollout. It reports whether it read to the end (or
// found the thread ineligible).
func readRollout(f *os.File, since time.Time, eligible Eligible, b *budget, skipped *int, threads map[string]*Session, parents map[string]string, facts *codexAccount) bool {
	meta, ok := rolloutMeta(f, b)
	if !ok || meta.Payload.ID == "" || meta.Payload.Cwd == "" || !eligible(meta.Payload.Cwd) {
		return true
	}
	id := meta.Payload.ID
	var spawn codexSpawn
	if len(meta.Payload.Source) > 0 && json.Unmarshal(meta.Payload.Source, &spawn) == nil {
		if p := spawn.Subagent.ThreadSpawn.ParentThreadID; p != "" && p != id {
			parents[id] = p
		}
	}
	info, err := f.Stat()
	if err != nil {
		return true
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
			if prev != nil && cur.Total == prev.Total {
				return true
			}
			d := cur
			if prev != nil && cur.Total > prev.Total {
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
		return done
	}
}

// rolloutMeta finds the session_meta among the rollout's first lines.
func rolloutMeta(f *os.File, b *budget) (codexMeta, bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return codexMeta{}, false
	}
	r := bufio.NewReaderSize(f, 1<<16)
	for k := 0; k < metaLines; k++ {
		line, n, err := readLine(r)
		b.bytes -= n
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
