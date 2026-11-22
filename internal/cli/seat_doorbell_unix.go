//go:build darwin || linux

package cli

// publishedWakeChannel treats an older or missing sidecar as a typing wake.
func (applied wakeSettingsAppliedFile) publishedWakeChannel() wakeChannel {
	switch applied.Doorbell {
	case wakeChannelNone, wakeChannelHook:
		return applied.Doorbell
	default:
		return wakeChannelTyped
	}
}

// readSeatDoorbell uses the published channel for exactly the inspected wake
// generation. Only a seat without a live wake uses its settings directly.
func readSeatDoorbell(root, me string) (ringer seatDoorbell, optIn bool) {
	var layers wakeSettingsLayers
	return layers.readSeatDoorbell(root, me)
}

// A long-running waiter retains the last valid settings across refused edits.
func (layers *wakeSettingsLayers) readSeatDoorbell(root, me string) (seatDoorbell, bool) {
	if observed, live := observeCoopHookWake(canonicalWakeRoot(root), me, false); live {
		return decideSeatDoorbell(true, observed.channel, observed.attended), observed.channel == wakeChannelHook
	}
	settings, next := coopWaitSettings(root, me, *layers)
	*layers = next
	return decideSeatDoorbell(false, "", settings.attended), settings.hookDoorbell
}
