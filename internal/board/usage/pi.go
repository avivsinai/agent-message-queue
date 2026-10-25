package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// piHeader is a pi session file's first line. A fork records the file it
// was copied from as parentSession and its own creation time as timestamp.
type piHeader struct {
	Type          string          `json:"type"`
	Version       json.RawMessage `json:"version"`
	ID            string          `json:"id"`
	Timestamp     time.Time       `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	ParentSession string          `json:"parentSession"`
}

// piLine is an assistant message entry. Only these entries are counted;
// "usage" entries and compaction usage would count the same tokens twice. A
// fork copies its parent's entries, id and timestamp unchanged, into its own
// file, so an entry counts once across the account's files, for the session
// that wrote it.
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
	entries := &piEntries{index: map[piEntry]int{}}
	var unknown string
	for _, fl := range files {
		if b.spent() {
			break
		}
		f, err := os.Open(fl.path)
		if err != nil {
			continue // unread: the totals say "at least"
		}
		switch readPiSession(f, since, eligible, b, &acct.Skipped, sessions, entries, &unknown) {
		case readDone:
			acct.Read++
		case readUnverified:
			acct.Unverified++
		}
		_ = f.Close()
	}
	for _, c := range entries.list {
		s := sessions[c.session]
		if s == nil {
			s = &Session{Harness: Pi, ID: c.session}
			sessions[c.session] = s
		}
		s.events = append(s.events, c.ev)
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

// piEntries holds each counted entry once, in read order, with the session
// it counts for. An entry a fork copied predates the fork's header; it
// counts for the file that holds it as its own, and while no such file was
// read it belongs to no session (ID ""), never to the fork.
type piEntries struct {
	index map[piEntry]int
	list  []piCount
}

type piCount struct {
	session string
	ev      event
}

func (e *piEntries) add(h piHeader, id string, ev event) {
	session := h.ID
	if h.ParentSession != "" && ev.at.Before(h.Timestamp) {
		session = ""
	}
	if id == "" {
		e.list = append(e.list, piCount{session, ev})
		return
	}
	k := piEntry{id, ev.at.UnixNano()}
	if i, ok := e.index[k]; ok {
		if e.list[i].session == "" {
			e.list[i].session = session
		}
		return
	}
	e.index[k] = len(e.list)
	e.list = append(e.list, piCount{session, ev})
}

func readPiSession(f *os.File, since time.Time, eligible Eligible, b *budget, skipped *int, sessions map[string]*Session, entries *piEntries, unknown *string) readOutcome {
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
	done := scan(f, seekWindow(f, info.Size(), since, b), piKeys, b, skipped, func(raw []byte) bool {
		var l piLine
		if json.Unmarshal(raw, &l) != nil || l.Type != "message" || l.Message.Role != "assistant" || l.Message.Usage == nil || l.Timestamp.Before(since) {
			return true
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
		entries.add(h, l.ID, event{at: l.Timestamp, Counts: c})
		return true
	})
	if done {
		return readDone
	}
	return readIncomplete
}
