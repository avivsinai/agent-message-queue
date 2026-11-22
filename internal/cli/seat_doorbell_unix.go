//go:build darwin || linux

package cli

// readSeatDoorbell uses the published channel for exactly the inspected wake
// generation. Only a seat without a live wake uses its settings directly.
func readSeatDoorbell(root, me string) seatDoorbellRead {
	var layers wakeSettingsLayers
	return layers.readSeatDoorbell(root, me)
}

// A long-running waiter retains the last valid settings across refused edits.
func (layers *wakeSettingsLayers) readSeatDoorbell(root, me string) seatDoorbellRead {
	if observed, live := observeCoopHookWake(canonicalWakeRoot(root), me, false); live {
		hook := observed.channel == wakeChannelHook
		return seatDoorbellRead{
			ringer:        decideSeatDoorbell(true, observed.channel, observed.attended),
			hookPublished: hook,
			optIn:         hook,
		}
	}
	settings, next := coopWaitSettings(root, me, *layers)
	*layers = next
	return seatDoorbellRead{
		ringer:   decideSeatDoorbell(false, "", settings.attended),
		optIn:    settings.hookDoorbell,
		settings: &settings,
	}
}
