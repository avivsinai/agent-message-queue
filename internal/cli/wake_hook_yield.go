package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	hookRingGrace = 2 * time.Minute
	// The waiter writes its rang IDs before attempting delivery. Its mtime
	// bounds the handover until the session arms its next waiter.
	coopWaitStateFile = ".rewake-hook-state.json"
)

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

// hookYield checks the waiter's lock and its last ring against pending, the
// inbox file names of the cohort the wake would announce. Both are delivery
// evidence; only a drain receipt establishes consumption. A ring at least
// hookRingGrace old that named a message still pending was not followed by
// a drain, so the wake types for it even while a waiter holds the lock.
func (cfg *wakeConfig) hookYield(now time.Time, pending map[string]os.FileInfo) (time.Duration, bool) {
	if cfg.publishedChannel != wakeChannelHook {
		return 0, false
	}
	reason := "no waiter and no recent ring; typing"
	var wait time.Duration
	rang, ringAt, ok := readHookRing(filepath.Join(fsq.AgentBase(cfg.root, cfg.me), coopWaitStateFile))
	age := now.Sub(ringAt)
	switch {
	case ok && age >= hookRingGrace && hookRingNamesAny(rang, pending):
		reason = "a ring was not followed by a drain within its grace period; typing"
	case coopWaitRuns(cfg.root, cfg.me):
		wait = wakeDoorbellRetryBase
		reason = "a waiter holds the doorbell"
	case ok && age >= 0 && age < hookRingGrace:
		wait = min(wakeDoorbellRetryBase, hookRingGrace-age)
		reason = "the last ring is within its grace period"
	}
	if cfg.hookYieldReason != reason {
		cfg.hookYieldReason = reason
		_ = writeWakeDiagnostic(cfg, "amq wake: hook doorbell: %s\n", reason)
	}
	return wait, wait > 0
}

// readHookRing reads the ids of the waiter's last ring and the record's
// file time, which is the time of that ring. The ids of every session
// count: the wake asks only whether a message it would announce was rung.
func readHookRing(path string) ([]string, time.Time, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, time.Time{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, info.ModTime(), true
	}
	defer func() { _ = f.Close() }()
	var record struct {
		RangIDs []string `json:"rang_ids"`
	}
	_ = json.NewDecoder(io.LimitReader(f, maxWakeMetadataFileBytes)).Decode(&record)
	return record.RangIDs, info.ModTime(), true
}

// hookRingNamesAny reports whether a rang id names an inbox file in pending.
func hookRingNamesAny(rang []string, pending map[string]os.FileInfo) bool {
	for _, id := range rang {
		if _, ok := pending[id+".md"]; id != "" && ok {
			return true
		}
	}
	return false
}

func (cfg *wakeConfig) preAttemptHold(now time.Time, reminder bool, pending map[string]os.FileInfo) (time.Duration, bool) {
	if wait, held := cfg.hookYield(now, pending); held {
		return wait, true
	}
	if reminder {
		return cfg.reminderHold(now)
	}
	return 0, false
}
