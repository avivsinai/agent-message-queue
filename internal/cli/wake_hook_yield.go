package cli

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	hookRingGrace = 2 * time.Minute
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
// announced it (the waiter rings it), or when the record that announced it
// is younger than hookRingGrace (the agent has the grace to drain). An
// announced message whose record is older, or dated in the future, was not
// followed by a drain, so the wake types for it even while a waiter holds
// the lock. An empty cohort is not covered. Every fact here is delivery
// evidence; only a drain receipt establishes consumption.
func (cfg *wakeConfig) hookYield(now time.Time, ids []string) (time.Duration, bool) {
	if cfg.publishedChannel != wakeChannelHook {
		return 0, false
	}
	waiter := coopWaitRuns(cfg.root, cfg.me)
	// The wake does not know the waiter's session, so a ring of any session
	// counts.
	announced := coopAnnounced(fsq.AgentBase(cfg.root, cfg.me), "", true)
	wait, reason := wakeDoorbellRetryBase, "a waiter holds the doorbell"
	if len(ids) == 0 {
		wait, reason = 0, "no mail with an id; typing"
	}
	for _, id := range ids {
		at, ok := announced[id]
		age := now.Sub(at)
		switch {
		case !ok && waiter:
		case ok && age >= 0 && age < hookRingGrace:
			wait = min(wait, hookRingGrace-age)
			if !waiter {
				reason = "a hook announced the mail within its grace period"
			}
		case ok:
			wait, reason = 0, "a hook announced mail that was not drained within its grace period; typing"
		default:
			wait, reason = 0, "no waiter for mail that no hook announced; typing"
		}
		if wait == 0 {
			break
		}
	}
	if cfg.hookYieldReason != reason {
		cfg.hookYieldReason = reason
		_ = writeWakeDiagnostic(cfg, "amq wake: hook doorbell: %s\n", reason)
	}
	return wait, wait > 0
}

// coopAnnounced is the announced set of the agent whose mailbox is agent:
// the ids of the waiter's last ring and the ids the Stop guard reported by
// a block. Each id maps to the file time of the record that holds it, the
// newer one when both do: the time the id was last announced. A ring heard
// by one session says nothing to another, so only the ring of session
// counts, unless anySession.
func coopAnnounced(agent, session string, anySession bool) map[string]time.Time {
	announced := make(map[string]time.Time)
	add := func(ids []string, at time.Time) {
		for _, id := range ids {
			if prev, ok := announced[id]; id != "" && (!ok || at.After(prev)) {
				announced[id] = at
			}
		}
	}
	var stop coopStopState
	if at, ok := readCoopHookRecord(filepath.Join(agent, coopStopStateFile), &stop); ok {
		add(stop.BlockedIDs, at)
	}
	var rang coopWaitState
	if at, ok := readCoopHookRecord(filepath.Join(agent, coopWaitStateFile), &rang); ok && (anySession || rang.ClaudeSession == session) {
		add(rang.RangIDs, at)
	}
	return announced
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
