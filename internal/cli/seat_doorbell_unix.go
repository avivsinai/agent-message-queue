//go:build darwin || linux

package cli

// seatWake is the lock-bound half of a seat read: one inspection of the
// wake lock and of the record the live wake published for its generation.
type seatWake struct {
	coopHookWakeObservation
	live bool
}

func observeSeatWake(root, me string) seatWake {
	obs, live := observeCoopHookWake(canonicalWakeRoot(root), me, false)
	return seatWake{coopHookWakeObservation: obs, live: live}
}

// ringer decides the seat under this wake. attended is the settings files'
// value; it counts only where the wake's record does not decide: with no
// live wake, or under a live wake whose record carries no doorbell value
// (an older wake, or a publication that failed for this generation).
// attended can only silence a ringer, so ringer(false) bounds every answer.
func (w seatWake) ringer(attended bool) seatDoorbell {
	if w.live && w.recorded {
		attended = w.attended
	}
	return decideSeatDoorbell(w.live, w.channel, attended)
}

// readSeatDoorbell is one read of who rings (root, me). It reads the
// settings files only when the live wake's record does not decide.
func readSeatDoorbell(root, me string) seatDoorbellRead {
	w := observeSeatWake(root, me)
	if w.live {
		hook := w.channel == wakeChannelHook
		read := seatDoorbellRead{hookPublished: hook, optIn: hook}
		if w.recorded {
			read.ringer = w.ringer(false)
		} else {
			settings, _ := coopWaitSettings(root, me, wakeSettingsLayers{})
			read.ringer = w.ringer(settings.attended)
		}
		return read
	}
	settings, _ := coopWaitSettings(root, me, wakeSettingsLayers{})
	return seatDoorbellRead{
		ringer: w.ringer(settings.attended),
		optIn:  settings.hookDoorbell,
	}
}
