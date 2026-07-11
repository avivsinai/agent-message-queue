package cli

const (
	presenceSourceNotifierLive   = "notifier_live"
	presenceSourceRecentActivity = "recent_activity"
)

func resolvePresenceSource(root, agent string, recentActivity bool) string {
	inspection := inspectWakeLock(root, agent)
	if inspection.Status == wakeLockValid && inspection.IdentityConfirmed {
		return presenceSourceNotifierLive
	}
	if recentActivity {
		return presenceSourceRecentActivity
	}
	return ""
}
