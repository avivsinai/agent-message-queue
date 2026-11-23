package cli

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	hookRingGrace = 2 * time.Minute
	// hookRecordSkew is how far a hook record's file time may be ahead of a
	// scan's now and still be a record written at now: the scan reads its
	// clock before it reads the records, which another process writes.
	hookRecordSkew = wakeDoorbellRetryBase
	// The waiter writes its rang IDs before attempting delivery, so the
	// file's time is the time of its last ring.
	coopWaitStateFile = ".rewake-hook-state.json"
)

// coopWaitState's ClaudeSession is the waiter's session key: the Claude
// session, or the Codex thread id. The field keeps its name so a record
// written by an earlier release reads the same.
type coopWaitState struct {
	Schema        int      `json:"schema"`
	ClaudeSession string   `json:"claude_session"`
	RangIDs       []string `json:"rang_ids"`
}

// wakeDoorbellChannel is the channel a wake publishes for settings. The
// inject mode and injector come from cfg alone, at startup and at every
// reload.
func wakeDoorbellChannel(settings wakeSettings, cfg *wakeConfig) wakeChannel {
	mode := effectiveInjectMode(cfg)
	switch {
	case settings.attended || mode == wakeInjectModeNone:
		return wakeChannelNone
	case settings.hookDoorbell && cfg.injectVia == "" && (mode == wakeInjectModeRaw || mode == wakeInjectModePaste):
		return wakeChannelHook
	default:
		return wakeChannelTyped
	}
}

func (cfg *wakeConfig) applyPublishedChannel(channel wakeChannel) {
	previous := cfg.publishedChannel
	cfg.publishedChannel = channel
	if previous == channel {
		return
	}
	cfg.hookYieldReason = ""
	if channel == wakeChannelHook || previous == wakeChannelHook {
		_ = writeWakeDiagnostic(cfg, "amq wake: published %s doorbell for this seat\n", channel)
	}
}

// hookYield reports whether the harness's own hook covers every message of
// ids, the header ids of the cohort the wake would announce, and when to
// look again. A message is covered when a waiter holds its lock and no hook
// announced it (the waiter rings it); when the record that announced it is
// younger than hookRingGrace (the agent has the grace to drain); or when no
// waiter holds the lock, no hook announced it, and the waiter's last ring
// of any message is younger than hookRingGrace (the turn that ring started
// runs, and its end starts the waiter that rings this message). A record
// time ahead of now by at most hookRecordSkew is a record written at now; a
// time further ahead counts as old. An announced message whose record is
// old was not followed by a drain, so the wake types for it even while a
// waiter holds the lock. An empty cohort is not covered, and neither is a
// cohort with a file that has no readable header id: the waiter cannot
// ring it. Every fact here is delivery evidence; only a drain receipt
// establishes consumption.
func (cfg *wakeConfig) hookYield(now time.Time, ids []string) (time.Duration, bool) {
	if cfg.publishedChannel != wakeChannelHook {
		return 0, false
	}
	waiter := coopWaitRuns(cfg.root, cfg.me)
	// The wake does not know the waiter's session, so a ring of any session
	// counts.
	seen := coopAnnounced(fsq.AgentBase(cfg.root, cfg.me), "", true)
	// grace is what is left of hookRingGrace after a record written at at.
	grace := func(at time.Time) time.Duration {
		if age, ok := hookRecordAge(now, at); ok && age < hookRingGrace {
			return hookRingGrace - age
		}
		return 0
	}
	wait, reason := wakeDoorbellRetryBase, "a waiter holds the doorbell"
	switch {
	case cfg.cohortUnreadable:
		wait, reason = 0, "unread mail has no readable header id; typing"
	case len(ids) == 0:
		wait, reason = 0, "no mail with an id; typing"
	}
	for _, id := range ids {
		if wait == 0 {
			break
		}
		at, announced := seen.ids[id]
		switch {
		case !announced && waiter:
		case announced && grace(at) > 0:
			wait = min(wait, grace(at))
			if !waiter {
				reason = "a hook announced the mail within its grace period"
			}
		case announced:
			wait, reason = 0, "a hook announced mail that was not drained within its grace period; typing"
		case grace(seen.rangAt) > 0:
			wait = min(wait, grace(seen.rangAt))
			reason = "a turn that a ring started runs; its end starts the waiter"
		default:
			wait, reason = 0, "no waiter for mail that no hook announced; typing"
		}
	}
	if cfg.hookYieldReason != reason {
		cfg.hookYieldReason = reason
		_ = writeWakeDiagnostic(cfg, "amq wake: hook doorbell: %s\n", reason)
	}
	return wait, wait > 0
}

// hookRecordAge is the age at now of a hook record whose file time is at.
// A time ahead of now by at most hookRecordSkew is a record written at now
// (age 0). A zero time, or a time further ahead, is no evidence: ok is
// false.
func hookRecordAge(now, at time.Time) (age time.Duration, ok bool) {
	age = now.Sub(at)
	if at.IsZero() || age < -hookRecordSkew {
		return 0, false
	}
	return max(age, 0), true
}

// coopAnnouncement is what the co-op hooks announced to an agent. ids maps
// each announced id to the time it was last announced: the file time of the
// record that holds it, the newer one when both do. rang is the waiter's
// rang record that counts, and rangAt the time of its last ring, zero when
// no record counts.
type coopAnnouncement struct {
	ids    map[string]time.Time
	rang   coopWaitState
	rangAt time.Time
}

// coopAnnounced reads the announced set of the agent whose mailbox is
// agent, each record once: the ids of the waiter's last ring and the ids
// the Stop guard reported by a block. A ring heard by one session says
// nothing to another, so only the ring of session counts, unless
// anySession; a rang record that does not count reads as an empty record of
// session.
func coopAnnounced(agent, session string, anySession bool) coopAnnouncement {
	seen := coopAnnouncement{
		ids:  make(map[string]time.Time),
		rang: coopWaitState{Schema: 1, ClaudeSession: session},
	}
	add := func(ids []string, at time.Time) {
		for _, id := range ids {
			if prev, ok := seen.ids[id]; id != "" && (!ok || at.After(prev)) {
				seen.ids[id] = at
			}
		}
	}
	var stop coopStopState
	if at, ok := readCoopHookRecord(filepath.Join(agent, coopStopStateFile), &stop); ok {
		add(stop.BlockedIDs, at)
	}
	var rang coopWaitState
	if at, ok := readCoopHookRecord(filepath.Join(agent, coopWaitStateFile), &rang); ok && (anySession || rang.ClaudeSession == session) {
		rang.Schema = 1
		seen.rang, seen.rangAt = rang, at
		add(rang.RangIDs, at)
	}
	return seen
}

// readCoopHookRecord decodes the co-op hook record at path into v and
// returns the file's time, which is when the hook last wrote it. A missing
// or undecodable record, a link, or a file that is not regular reads as
// none. The read has no size bound: a record holds one id per unread
// message, and a record cut short would read as none, so the waiter would
// ring every message of a large backlog again.
func readCoopHookRecord(path string, v any) (time.Time, bool) {
	f, info, err := fsq.OpenRegularNoFollow(path)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	if json.NewDecoder(f).Decode(v) != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func (cfg *wakeConfig) preAttemptHold(now time.Time, reminder bool, ids []string) (time.Duration, bool) {
	if wait, held := cfg.hookYield(now, ids); held {
		return wait, true
	}
	if reminder {
		return cfg.reminderHold(now)
	}
	return 0, false
}
