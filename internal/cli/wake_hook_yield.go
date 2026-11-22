package cli

import (
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

func wakeDoorbellChannel(settings wakeSettings, mode, injectVia string) wakeChannel {
	switch {
	case settings.attended || mode == wakeInjectModeNone:
		return wakeChannelNone
	case settings.hookDoorbell && injectVia == "" && (mode == wakeInjectModeRaw || mode == wakeInjectModePaste):
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

// hookYield checks the waiter's lock and the time of its last ring. Both
// are delivery evidence; only a drain receipt establishes consumption.
func (cfg *wakeConfig) hookYield(now time.Time) (time.Duration, bool) {
	if cfg.publishedChannel != wakeChannelHook {
		return 0, false
	}
	reason := "no waiter and no recent ring; typing"
	var wait time.Duration
	if coopWaitRuns(cfg.root, cfg.me) {
		wait = wakeDoorbellRetryBase
		reason = "a waiter holds the doorbell"
	} else if info, err := os.Lstat(filepath.Join(fsq.AgentBase(cfg.root, cfg.me), coopWaitStateFile)); err == nil && info.Mode().IsRegular() {
		age := now.Sub(info.ModTime())
		if age >= 0 && age < hookRingGrace {
			wait = min(wakeDoorbellRetryBase, hookRingGrace-age)
			reason = "the last ring is within its grace period"
		}
	}
	if cfg.hookYieldReason != reason {
		cfg.hookYieldReason = reason
		_ = writeWakeDiagnostic(cfg, "amq wake: hook doorbell: %s\n", reason)
	}
	return wait, wait > 0
}

func (cfg *wakeConfig) preAttemptHold(now time.Time, reminder bool) (time.Duration, bool) {
	if wait, held := cfg.hookYield(now); held {
		return wait, true
	}
	if reminder {
		return cfg.reminderHold(now)
	}
	return 0, false
}
