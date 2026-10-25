package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// piHeader is a pi session file's first line.
type piHeader struct {
	Type    string          `json:"type"`
	Version json.RawMessage `json:"version"`
	ID      string          `json:"id"`
	Cwd     string          `json:"cwd"`
}

// piLine is an assistant message entry. Only these entries are counted;
// "usage" entries and compaction usage would count the same tokens twice. A
// fork copies its parent's entries, id and timestamp unchanged, into its own
// file, so an entry counts once across the account's files.
// cost.total is the provider's billed cost when totalFromProvider is true,
// otherwise a catalog estimate; a total of 0 with no flag reports no cost.
type piLine struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Role  string `json:"role"`
		Usage *struct {
			In         int64 `json:"input"`
			Out        int64 `json:"output"`
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
			Cost       *struct {
				Total             float64 `json:"total"`
				TotalFromProvider bool    `json:"totalFromProvider"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

var piKeys = [][]byte{[]byte(`"assistant"`)}

const piNoLimits = "limit unknown (pi records no plan limits)"

func readPi(acct *Account, files []file, now time.Time, eligible Eligible, b *budget) {
	since := now.Add(-span)
	sessions := map[string]*Session{}
	seen := map[piEntry]bool{}
	var unknown string
	for _, fl := range files {
		if b.spent() {
			break
		}
		f, err := os.Open(fl.path)
		if err != nil {
			continue // unread: the totals say "at least"
		}
		switch readPiSession(f, since, eligible, b, &acct.Skipped, sessions, seen, &unknown) {
		case readDone:
			acct.Read++
		case readUnverified:
			acct.Unverified++
		}
		_ = f.Close()
	}
	acct.Sessions = sortedSessions(sessions)
	reason := piNoLimits
	if unknown != "" {
		reason = "limit unknown (" + unknown + ")"
	}
	acct.Limits = append(acct.Limits, Limit{Kind: "unknown", Source: "none", Reason: reason})
}

// piEntry identifies an entry across files: ids are short, so the timestamp
// keeps two sessions' entries apart.
type piEntry struct {
	id string
	at int64 // Unix nanoseconds: one instant, whatever its zone
}

func readPiSession(f *os.File, since time.Time, eligible Eligible, b *budget, skipped *int, sessions map[string]*Session, seen map[piEntry]bool, unknown *string) readOutcome {
	r := bufio.NewReaderSize(f, 1<<16)
	line, _ := readLine(r, b)
	var h piHeader
	if line == nil || json.Unmarshal(line, &h) != nil || h.Type != "session" || h.ID == "" || h.Cwd == "" {
		return readIncomplete
	}
	if !eligible(h.Cwd) {
		return readDone
	}
	if v := string(h.Version); v != strconv.Itoa(piFormat) {
		s := sessions[h.ID]
		if s == nil {
			s = &Session{Harness: Pi, ID: h.ID}
			sessions[h.ID] = s
		}
		s.Unknown = unsupported("pi session format", strings.Trim(v, `"`))
		if *unknown == "" {
			*unknown = s.Unknown
		}
		return readUnverified
	}
	info, err := f.Stat()
	if err != nil {
		return readIncomplete
	}
	var evs []event
	done := scan(f, seekWindow(f, info.Size(), since, b), piKeys, b, skipped, func(raw []byte) bool {
		var l piLine
		if json.Unmarshal(raw, &l) != nil || l.Type != "message" || l.Message.Role != "assistant" || l.Message.Usage == nil || l.Timestamp.Before(since) {
			return true
		}
		if l.ID != "" {
			k := piEntry{l.ID, l.Timestamp.UnixNano()}
			if seen[k] {
				return true
			}
			seen[k] = true
		}
		u := l.Message.Usage
		c := Counts{In: u.In, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Out: u.Out}
		if u.Cost != nil && (u.Cost.Total != 0 || u.Cost.TotalFromProvider) {
			total := u.Cost.Total
			c.Cost, c.CostKind = &total, "catalog estimate"
			if u.Cost.TotalFromProvider {
				c.CostKind = "provider-billed"
			}
		}
		evs = append(evs, event{at: l.Timestamp, Counts: c})
		return true
	})
	if len(evs) > 0 {
		s := sessions[h.ID]
		if s == nil {
			s = &Session{Harness: Pi, ID: h.ID}
			sessions[h.ID] = s
		}
		s.events = append(s.events, evs...)
	}
	if done {
		return readDone
	}
	return readIncomplete
}
