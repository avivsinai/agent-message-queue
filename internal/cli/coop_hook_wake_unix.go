//go:build darwin || linux

package cli

import (
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// coopHookWakeObservation is one read of the wake lock and prepared marker.
type coopHookWakeObservation struct {
	pid        int
	generation string
	baselineAt string
	channel    wakeChannel
	attended   bool
}

// coopHookWakeBaseline returns the startup baseline of the live wake for
// (root, me), with amq wake check's liveness rule and baseline_at, whatever
// channel that wake published. ok is false when no wake is live, the live
// wake has published no baseline, or the lock changed between two reads;
// the guard then counts every message.
func coopHookWakeBaseline(root, me string) (time.Time, bool) {
	root = canonicalWakeRoot(root)
	first, ok := observeCoopHookWake(root, me, true)
	if !ok {
		return time.Time{}, false
	}
	if second, ok := observeCoopHookWake(root, me, true); !ok || second != first {
		return time.Time{}, false
	}
	baseline, err := time.Parse(time.RFC3339Nano, first.baselineAt)
	return baseline, err == nil
}

func observeCoopHookWake(root, me string, withBaseline bool) (coopHookWakeObservation, bool) {
	var obs coopHookWakeObservation
	agentDir, err := openWakeDirectory(fsq.AgentBase(root, me), "wake agent directory")
	if err != nil {
		return obs, false
	}
	defer func() { _ = agentDir.Close() }()
	live := false
	if err := agentDir.withFD(func(dirfd int) error {
		inspection := inspectWakeLockAt(dirfd, agentDir, root, me)
		live = inspection.Exists &&
			inspection.Status == wakeLockValid &&
			inspection.IdentityConfirmed &&
			inspection.Process.Running
		if !live {
			return nil
		}
		applied, _ := readWakeSettingsAppliedAt(dirfd, agentDir, inspection)
		obs = coopHookWakeObservation{
			channel:    publishedWakeChannel(applied.Doorbell, inspection.Lock.WakeMode),
			attended:   applied.Attended,
			pid:        inspection.PID,
			generation: inspection.Lock.Generation,
		}
		if withBaseline {
			obs.baselineAt = observeWakeCheckBaselineAt(dirfd, agentDir, inspection)
		}
		return nil
	}); err != nil {
		return obs, false
	}
	return obs, live
}
