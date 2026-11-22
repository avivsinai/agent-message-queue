package cli

// wakeChannel is the live wake's generation-bound decision about its output.
type wakeChannel string

const (
	wakeChannelTyped wakeChannel = "typed"
	wakeChannelHook  wakeChannel = "hook"
	wakeChannelNone  wakeChannel = "none"
)

type seatDoorbell uint8

const (
	doorbellTyped seatDoorbell = iota
	doorbellHook
	doorbellNone
)

// seatDoorbellRead is one read of who rings a seat. hookPublished is true
// only when a live wake published the hook channel for its generation.
// optIn is whether the seat takes the hook doorbell: hookPublished under a
// live wake, else the seat's hook_doorbell setting.
type seatDoorbellRead struct {
	ringer        seatDoorbell
	hookPublished bool
	optIn         bool
}

// decideSeatDoorbell selects the sender of the automatic mail notification.
func decideSeatDoorbell(wakeLive bool, published wakeChannel, attended bool) seatDoorbell {
	switch {
	case attended:
		return doorbellNone
	case !wakeLive, published == wakeChannelHook, published == wakeChannelNone:
		return doorbellHook
	default:
		return doorbellTyped
	}
}

// publishedWakeChannel is the channel a live wake publishes for its lock
// generation: the recorded channel when it names hook or none, else the
// lock's own mode, which binds to the same generation. A notify-only lock
// types nothing, so with no recorded channel it reads as none.
func publishedWakeChannel(recorded wakeChannel, lockMode string) wakeChannel {
	switch {
	case recorded == wakeChannelHook, recorded == wakeChannelNone:
		return recorded
	case lockMode == wakeInjectModeNone:
		return wakeChannelNone
	default:
		return wakeChannelTyped
	}
}
