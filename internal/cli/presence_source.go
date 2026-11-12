package cli

const (
	presenceSourceNotifierLive   = "notifier_live"
	presenceSourceHookLive       = "hook_live"
	presenceSourceRecentActivity = "recent_activity"
)

// resolvePresenceSource says why an agent counts as active, or "" when
// nothing shows that it is: a live wake rings it, a wait hook of its Claude
// Code session holds its doorbell, or its presence is recent.

func resolvePresenceSource(root, agent string, recentActivity bool) string {
	inspection := inspectWakeLock(root, agent)
	if inspection.Status == wakeLockValid && inspection.IdentityConfirmed {
		return presenceSourceNotifierLive
	}
	if coopWaitRuns(root, agent) {
		return presenceSourceHookLive
	}
	if recentActivity {
		return presenceSourceRecentActivity
	}
	return ""
}
