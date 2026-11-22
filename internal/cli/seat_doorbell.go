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
