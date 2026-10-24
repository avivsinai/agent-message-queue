package usage

import (
	"bytes"
	"encoding/json"
	"os"
	"time"
)

// claudeLine is the part of a Claude Code transcript line the reader keeps.
// Subagent files carry their parent's sessionId, so a subagent's spend rolls
// up to the parent by a recorded field. One API message is written once per
// content block with the same usage, so each message.id counts once.
type claudeLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
	Cwd       string    `json:"cwd"`
	Version   string    `json:"version"`
	Message   struct {
		ID    string `json:"id"`
		Usage *struct {
			In         int64 `json:"input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			Out        int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	QuotaLimits *struct {
		Status        string `json:"status"`
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"quotaLimits"`
}

var claudeUsageKeys = [][]byte{[]byte(`"usage"`), []byte(`"quotaLimits"`)}

// claudeCwd extracts a transcript line's working directory.
func claudeCwd(line []byte) (string, bool) {
	if !bytes.Contains(line, []byte(`"cwd"`)) {
		return "", false
	}
	var l struct {
		Cwd string `json:"cwd"`
	}
	if json.Unmarshal(line, &l) != nil || l.Cwd == "" {
		return "", false
	}
	return l.Cwd, true
}

// claudeRelayOff is what the account shows without a plan figure: Claude
// Code writes plan usage only to its status line.
const claudeRelayOff = "limit unknown (status line relay off)"

func readClaude(acct *Account, files []file, now time.Time, eligible Eligible, b *budget) {
	sessions := map[string]*Session{}
	seen := map[string]map[string]bool{}
	var reached *Limit
	var unknown string
	since := now.Add(-span)
	session := func(id string) *Session {
		s := sessions[id]
		if s == nil {
			s = &Session{Harness: Claude, ID: id}
			sessions[id] = s
		}
		return s
	}
	for _, fl := range files {
		if b.spent() {
			break
		}
		// A file that cannot be opened or identified stays unread, so the
		// totals say "at least".
		f, err := os.Open(fl.path)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			continue
		}
		// Another project's session is read no further than its first
		// working directory.
		cwd, found, err := headField(f, b, claudeCwd)
		if err != nil || !found {
			_ = f.Close()
			continue
		}
		if !eligible(cwd) {
			_ = f.Close()
			acct.Read++
			continue
		}
		verified := true
		done := scan(f, seekWindow(f, info.Size(), since, b), claudeUsageKeys, b, &acct.Skipped, func(raw []byte) bool {
			var l claudeLine
			if json.Unmarshal(raw, &l) != nil {
				return true
			}
			if l.Type != "assistant" || l.SessionID == "" || l.Timestamp.Before(since) {
				return true
			}
			// A line of an unverified version is not counted, neither its
			// tokens nor its limit.
			if !claudeVersions.has(l.Version) {
				verified = false
				s := session(l.SessionID)
				if s.Unknown == "" {
					s.Unknown = unsupported("Claude Code", l.Version)
				}
				if unknown == "" {
					unknown = s.Unknown
				}
				return true
			}
			if q := l.QuotaLimits; q != nil && q.Status == "rejected" && q.ResetsAt > 0 {
				at := time.Unix(q.ResetsAt, 0)
				if reached == nil || l.Timestamp.After(reached.AsOf) {
					reached = &Limit{Kind: "reached", Name: q.RateLimitType, ResetsAt: &at, AsOf: l.Timestamp, Source: "transcript-rejection"}
				}
			}
			u := l.Message.Usage
			if u == nil || l.Message.ID == "" {
				return true
			}
			ids := seen[l.SessionID]
			if ids == nil {
				ids = map[string]bool{}
				seen[l.SessionID] = ids
			}
			if ids[l.Message.ID] {
				return true
			}
			ids[l.Message.ID] = true
			s := session(l.SessionID)
			s.events = append(s.events, event{at: l.Timestamp, Counts: Counts{In: u.In, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Out: u.Out}})
			return true
		})
		_ = f.Close()
		switch {
		case !verified:
			acct.Unverified++
		case done:
			acct.Read++
		}
	}
	acct.Sessions = sortedSessions(sessions)
	// A rejection names a limit only while its reset is ahead.
	switch {
	case reached != nil && reached.ResetsAt.After(now):
		acct.Limits = append(acct.Limits, *reached)
	case unknown != "":
		acct.Limits = append(acct.Limits, Limit{Kind: "unknown", Source: "none", Reason: "limit unknown (" + unknown + ")"})
	default:
		acct.Limits = append(acct.Limits, Limit{Kind: "unknown", Source: "none", Reason: claudeRelayOff})
	}
}
